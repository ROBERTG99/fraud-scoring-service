package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// gatedClient blocks every call until release is closed, recording batch sizes; score = row[0].
type gatedClient struct {
	mu      sync.Mutex
	sizes   []int
	aliases []string
	release chan struct{}
	err     error
	short   bool
}

func (g *gatedClient) Predict(ctx context.Context, alias string, rows [][]float32) (Prediction, error) {
	g.mu.Lock()
	g.sizes = append(g.sizes, len(rows))
	g.aliases = append(g.aliases, alias)
	g.mu.Unlock()
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return Prediction{}, ErrUnavailable
		}
	}
	if g.err != nil {
		return Prediction{}, g.err
	}
	out := make([]float32, len(rows))
	for i, r := range rows {
		out[i] = r[0]
	}
	if g.short {
		out = out[:len(out)-1]
	}
	return Prediction{Scores: out, Version: "v-" + alias}, nil
}

func (g *gatedClient) calls() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.sizes) }

func TestBatcherCoalescesWhileCallsAreBusy(t *testing.T) {
	g := &gatedClient{release: make(chan struct{})}
	b := NewBatcher(g, BatchConfig{MaxBatch: 64, MaxInFlight: 1})
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const n = 100
	var wg sync.WaitGroup
	got := make([]Prediction, n)
	errs := make([]error, n)
	wg.Add(1)
	go func() { defer wg.Done(); got[0], errs[0] = b.Predict(ctx, AliasProduction, [][]float32{{0}}) }()
	for g.calls() == 0 { // the first request is sent immediately and holds the only slot
		if ctx.Err() != nil {
			t.Fatal("first call never reached the model")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = b.Predict(ctx, AliasProduction, [][]float32{{float32(i)}})
		}(i)
	}
	q, _ := b.queue(AliasProduction)
	for len(q) < n-2 { // the dispatcher holds one request while it waits for the slot; the rest queue
		if ctx.Err() != nil {
			t.Fatalf("only %d requests queued; calls %v", len(q), g.sizes)
		}
		time.Sleep(time.Millisecond)
	}
	close(g.release)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || len(got[i].Scores) != 1 || got[i].Scores[0] != float32(i) || got[i].Version != "v-production" {
			t.Fatalf("request %d: %+v %v", i, got[i], errs[i])
		}
	}
	// 1 immediate call, then the 99 queued requests in batches of at most 64.
	if g.calls() != 3 || g.sizes[0] != 1 || g.sizes[1] != 64 || g.sizes[2] != 35 {
		t.Fatalf("batch sizes %v", g.sizes)
	}
}

func TestBatcherSeparatesAliasesAndPropagatesErrors(t *testing.T) {
	g := &gatedClient{err: errors.New("boom")}
	b := NewBatcher(g, BatchConfig{})
	defer b.Close()
	if _, err := b.Predict(context.Background(), AliasShadow, [][]float32{{1}}); err == nil || err.Error() != "boom" {
		t.Fatalf("error: %v", err)
	}
	g.err = nil
	p, err := b.Predict(context.Background(), AliasShadow, [][]float32{{7}})
	if err != nil || p.Scores[0] != 7 || p.Version != "v-shadow" {
		t.Fatalf("shadow: %+v %v", p, err)
	}
	g.short = true
	if _, err := b.Predict(context.Background(), AliasProduction, [][]float32{{1}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("short response: %v", err)
	}
	g.short = false
	big := make([][]float32, 64)
	for i := range big {
		big[i] = []float32{1}
	}
	if p, err := b.Predict(context.Background(), AliasProduction, big); err != nil || len(p.Scores) != 64 {
		t.Fatalf("full batch bypass: %v", err)
	}
}

func TestBatcherHonoursCallerDeadlineAndClose(t *testing.T) {
	g := &gatedClient{release: make(chan struct{})}
	b := NewBatcher(g, BatchConfig{MaxInFlight: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := b.Predict(ctx, AliasProduction, [][]float32{{1}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("blocked model: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("caller deadline not honoured")
	}
	// A request without a deadline gets the 20 ms model default.
	if _, err := b.Predict(context.Background(), AliasProduction, [][]float32{{1}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("default deadline: %v", err)
	}
	close(g.release)
	b.Close()
	b.Close()
	if _, err := b.Predict(context.Background(), AliasProduction, [][]float32{{1}}); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
}
