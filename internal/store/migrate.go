// Package store persists decisions, cases, labels, shadow comparisons and the audit log in Postgres.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

	"github.com/ROBERTG99/fraud-scoring-service/migrations"
)

func provider(dsn string) (*goose.Provider, *sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return p, db, nil
}

// MigrateUp applies every pending migration.
func MigrateUp(ctx context.Context, dsn string) error {
	p, db, err := provider(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDownAll rolls back every migration.
func MigrateDownAll(ctx context.Context, dsn string) error {
	p, db, err := provider(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := p.DownTo(ctx, 0); err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// MigrationVersion returns the current schema version.
func MigrationVersion(ctx context.Context, dsn string) (int64, error) {
	p, db, err := provider(dsn)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return p.GetDBVersion(ctx)
}
