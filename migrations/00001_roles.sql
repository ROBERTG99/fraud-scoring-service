-- Application roles. Roles are cluster-wide, so Down leaves them in place (other databases may use them).
-- Development passwords only; production credentials come from the environment, never from migrations.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fraud_app') THEN
    BEGIN
      CREATE ROLE fraud_app LOGIN PASSWORD 'fraud_app';
    EXCEPTION WHEN duplicate_object OR unique_violation THEN
      NULL; -- created at the same moment by another database's migration (roles are cluster-wide)
    END;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fraud_readonly') THEN
    BEGIN
      CREATE ROLE fraud_readonly LOGIN PASSWORD 'fraud_readonly';
    EXCEPTION WHEN duplicate_object OR unique_violation THEN
      NULL; -- created at the same moment by another database's migration (roles are cluster-wide)
    END;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
