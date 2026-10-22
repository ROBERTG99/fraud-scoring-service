//go:build integration

package model

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

// TestTritonGoldenVector runs only with the optional `triton` compose profile (FRAUD_TEST_TRITON_URL).
// The Triton image (nvcr.io) is unreachable in the build environment; the skip reason is recorded in
// docs/KNOWN_LIMITATIONS.md. make verify never needs Triton.
func TestTritonGoldenVector(t *testing.T) {
	url := os.Getenv("FRAUD_TEST_TRITON_URL")
	if url == "" {
		t.Skip("Triton profile not running (FRAUD_TEST_TRITON_URL unset); see docs/KNOWN_LIMITATIONS.md")
	}
	g := loadGolden(t)
	rows := make([][]float32, len(g.Vectors))
	for i, v := range g.Vectors {
		rows[i] = v.Features
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := NewTriton(url, nil).Predict(ctx, AliasProduction, rows)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range g.Vectors {
		if d := math.Abs(float64(p.Scores[i]) - v.Probability); d > 1e-5 {
			t.Errorf("vector %d: triton %v, golden %v", i, p.Scores[i], v.Probability)
		}
	}
}
