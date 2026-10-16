-- Append-only, hash-chained audit log. fraud_app may only SELECT and INSERT; a trigger rejects
-- UPDATE, DELETE and TRUNCATE for every role, including owners.

-- +goose Up
CREATE TABLE audit_log (
  seq       bigserial PRIMARY KEY,
  ts        timestamptz NOT NULL DEFAULT now(),
  actor_sub text NOT NULL,
  role      text NOT NULL,
  action    text NOT NULL,
  target    text NOT NULL,
  details   jsonb NOT NULL,
  prev_hash text NOT NULL,
  hash      text NOT NULL UNIQUE
);
CREATE INDEX audit_log_ts_idx ON audit_log (ts DESC);

-- +goose StatementBegin
CREATE FUNCTION audit_log_reject_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_log is append-only (% rejected)', TG_OP USING ERRCODE = 'insufficient_privilege';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION audit_log_reject_change();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_reject_change();

GRANT SELECT, INSERT ON audit_log TO fraud_app;
GRANT USAGE, SELECT ON SEQUENCE audit_log_seq_seq TO fraud_app;
GRANT SELECT ON audit_log TO fraud_readonly;

-- +goose Down
DROP TABLE audit_log;
DROP FUNCTION audit_log_reject_change();
