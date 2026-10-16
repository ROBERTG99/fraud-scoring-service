package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type stubClient struct {
	mu      sync.Mutex
	block   chan struct{}
	err     error
	shadow  float32
	version string
}

func (s *stubClient) Predict(ctx context.Context, alias string, rows [][]float32) (Prediction, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return Prediction{}, ErrUnavailable
		}
	}
	if s.err != nil {
		return Prediction{}, s.err
	}
	return Prediction{Scores: []float32{s.shadow}, Version: s.version}, nil
}

type memSink struct {
	mu   sync.Mutex
	rows []Comparison
}

func (m *memSink) InsertComparison(_ context.Context, c Comparison) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, c)
	return nil
}

func band(s float64) string {
	switch {
	case s >= 0.85:
		return "DECLINE"
	case s >= 0.3:
		return "REVIEW"
	}
	return "APPROVE"
}

func TestShadowWritesComparisons(t *testing.T) {
	sink := &memSink{}
	r := NewShadowRunner(&stubClient{shadow: 0.9, version: "fraud-gbdt/2"}, sink, band, ShadowConfig{Workers: 2, Queue: 10}, nil)
	r.Submit(ShadowJob{DecisionID: "d1", Features: make([]float32, 13), PrimaryScore: 0.4, PrimaryVersion: "fraud-gbdt/1", PrimaryOK: true})
	r.Submit(ShadowJob{DecisionID: "d2", PrimaryOK: false}) // primary unavailable: nothing to compare
	r.Close()
	if len(sink.rows) != 1 {
		t.Fatalf("rows %d", len(sink.rows))
	}
	c := sink.rows[0]
	if c.PrimaryBand != "REVIEW" || c.ShadowBand != "DECLINE" || c.Agree || c.ShadowVersion != "fraud-gbdt/2" {
		t.Fatalf("comparison %+v", c)
	}
}

func TestShadowDropsWhenQueueFull(t *testing.T) {
	block := make(chan struct{})
	r := NewShadowRunner(&stubClient{block: block}, &memSink{}, band, ShadowConfig{Workers: 1, Queue: 2, Timeout: time.Second}, nil)
	start := time.Now()
	for i := 0; i < 50; i++ {
		r.Submit(ShadowJob{DecisionID: "d", Features: make([]float32, 13), PrimaryOK: true})
	}
	if time.Since(start) > 10*time.Millisecond {
		t.Fatalf("Submit blocked for %v", time.Since(start))
	}
	close(block)
	r.Close()
	done, _, dropped := r.Stats()
	if dropped < 47 || done+dropped != 50 {
		t.Fatalf("done %d dropped %d", done, dropped)
	}
}

func TestShadowErrorsAreCounted(t *testing.T) {
	r := NewShadowRunner(&stubClient{err: errors.New("boom")}, &memSink{}, band, ShadowConfig{Workers: 4, Queue: 100}, nil)
	for i := 0; i < 20; i++ {
		r.Submit(ShadowJob{DecisionID: "d", Features: make([]float32, 13), PrimaryOK: true})
	}
	r.Close()
	if _, failed, _ := r.Stats(); failed != 20 {
		t.Fatalf("failed %d", failed)
	}
}
