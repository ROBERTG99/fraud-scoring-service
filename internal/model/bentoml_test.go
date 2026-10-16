package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type golden struct {
	ModelVersion string `json:"model_version"`
	Vectors      []struct {
		Features    []float32 `json:"features"`
		Probability float64   `json:"probability"`
	} `json:"vectors"`
}

func loadGolden(t testing.TB) golden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "model", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestClientTimeoutReturnsErrUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"scores":[0.5],"model_version":"v"}`))
	}))
	defer srv.Close()
	c := NewBentoML(srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Predict(ctx, AliasProduction, [][]float32{make([]float32, 13)})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if elapsed > 30*time.Millisecond {
		t.Fatalf("timeout not honoured: returned after %v", elapsed)
	}
}

func TestClientErrorsWrapUnavailable(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"status":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusServiceUnavailable) },
		"garbage": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) },
		"mismatch": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"scores":[],"model_version":"v"}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			_, err := NewBentoML(srv.URL, nil).Predict(context.Background(), AliasProduction, [][]float32{make([]float32, 13)})
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("want ErrUnavailable, got %v", err)
			}
		})
	}
	_, err := NewBentoML("http://127.0.0.1:1", nil).Predict(context.Background(), AliasProduction, [][]float32{make([]float32, 13)})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("connection refused: want ErrUnavailable, got %v", err)
	}
}

func TestRequestShape(t *testing.T) {
	var got predictRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/predict" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"scores":[0.25],"model_version":"fraud-gbdt/3"}`))
	}))
	defer srv.Close()
	p, err := NewBentoML(srv.URL+"/", nil).Predict(context.Background(), AliasShadow, [][]float32{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != AliasShadow || len(got.Inputs) != 1 || got.Inputs[0][12] != 13 {
		t.Fatalf("request %+v", got)
	}
	if p.Version != "fraud-gbdt/3" || p.Scores[0] != 0.25 {
		t.Fatalf("prediction %+v", p)
	}
}

func TestDefaultDeadlineWithoutContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer srv.Close()
	start := time.Now()
	_, err := NewBentoML(srv.URL, nil).Predict(context.Background(), AliasProduction, [][]float32{make([]float32, 13)})
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("no default deadline: %v after %v", err, time.Since(start))
	}
}
