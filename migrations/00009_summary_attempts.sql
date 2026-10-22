-- Paid summary attempts per message (security audit, D-048). The case summarizer records an attempt
-- before each provider call and stops paying for a decision after three, so a failure after the call (persist,
-- save_steps) cannot buy summaries forever. Pruned with processed_messages (90 days, cmd/retention).

-- +goose Up
CREATE TABLE summary_attempts (
  consumer        text NOT NULL,
  message_key     text NOT NULL,
  attempts        integer NOT NULL DEFAULT 1 CHECK (attempts > 0),
  last_attempt_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, message_key)
);
CREATE INDEX summary_attempts_last_idx ON summary_attempts (last_attempt_at);
GRANT SELECT, INSERT, UPDATE ON summary_attempts TO fraud_summarizer;
GRANT SELECT, DELETE ON summary_attempts TO fraud_retention;

-- +goose Down
DROP TABLE summary_attempts;
