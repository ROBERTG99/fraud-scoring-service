-- Application roles. Roles are cluster-wide, so Down leaves them in place (other databases may use them).
-- Development passwords only; production credentials come from the environment, never from migrations.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fraud_app') THEN
    CREATE ROLE fraud_app LOGIN PASSWORD 'fraud_app';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fraud_readonly') THEN
    CREATE ROLE fraud_readonly LOGIN PASSWORD 'fraud_readonly';
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
