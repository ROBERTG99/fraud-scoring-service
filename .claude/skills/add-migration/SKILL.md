---
name: add-migration
description: Add a goose SQL migration for Postgres. Use when the data model changes.
---
# Add a migration

1. Create `migrations/NNNNN_<name>.sql` with `-- +goose Up` and `-- +goose Down` sections.
2. Grants: `fraud_app` gets only what it needs; never UPDATE/DELETE on `audit_log`.
3. Add or extend the integration test in `internal/store/migrations_test.go` (up/down/up).
4. Run `make test-go` (needs `make test-deps-up`).
5. Update `docs/architecture.md` data model if columns changed.
