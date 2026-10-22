-- Regenerate requests (security audit, D-054). The summarizer counts a user's paid regenerations here,
-- so the 20-per-hour limit holds across processes and restarts, and the idempotency key of a request lives
-- with it (the same key on another case is a different request). Pruned after 90 days (cmd/retention).

-- +goose Up
CREATE TABLE summary_regenerations (
  case_id         uuid NOT NULL,
  idempotency_key text NOT NULL,
  subject         text NOT NULL,
  requested_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (case_id, idempotency_key)
);
CREATE INDEX summary_regenerations_subject_idx ON summary_regenerations (subject, requested_at);
CREATE INDEX summary_regenerations_requested_idx ON summary_regenerations (requested_at);
GRANT SELECT, INSERT ON summary_regenerations TO fraud_summarizer;
GRANT SELECT, DELETE ON summary_regenerations TO fraud_retention;

-- +goose Down
DROP TABLE summary_regenerations;
