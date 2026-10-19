package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
)

// ShadowStore writes shadow_comparisons rows.
type ShadowStore struct{ pool *pgxpool.Pool }

// NewShadowStore returns the store.
func NewShadowStore(pool *pgxpool.Pool) *ShadowStore { return &ShadowStore{pool: pool} }

// InsertComparison stores one comparison; a decision that is still in the spool (FK miss) fails and the
// comparison is dropped (shadow results never block or retry).
func (s *ShadowStore) InsertComparison(ctx context.Context, c model.Comparison) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO shadow_comparisons (decision_id, primary_version, shadow_version, primary_score, shadow_score,
		primary_band, shadow_band, agree, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (decision_id) DO NOTHING`,
		c.DecisionID, c.PrimaryVersion, c.ShadowVersion, c.PrimaryScore, c.ShadowScore, c.PrimaryBand, c.ShadowBand, c.Agree, c.CreatedAt)
	return err
}
