//go:build integration

package model

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

// TestGoldenVectorScore: the Go client against the real BentoML server returns the Python/ONNX scores
// of the golden vectors within 1e-5.
func TestGoldenVectorScore(t *testing.T) {
	g := loadGolden(t)
	c := NewBentoML(testutil.ModelServerURL(t), nil)
	rows := make([][]float32, len(g.Vectors))
	for i, v := range g.Vectors {
		rows[i] = v.Features
	}
	for _, alias := range []string{AliasProduction, AliasShadow} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		p, err := c.Predict(ctx, alias, rows)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range g.Vectors {
			if d := math.Abs(float64(p.Scores[i]) - v.Probability); d > 1e-5 {
				t.Errorf("%s vector %d: score %v, golden %v (diff %.2e)", alias, i, p.Scores[i], v.Probability, d)
			}
		}
	}
}
