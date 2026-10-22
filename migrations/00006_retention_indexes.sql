-- Indexes for the retention job: each batch deletes the oldest rows
-- by these timestamps, which otherwise need a full scan per batch.

-- +goose Up
CREATE INDEX decisions_decided_idx ON decisions (decided_at);
CREATE INDEX account_events_ts_idx ON account_events (event_ts);
CREATE INDEX processed_messages_processed_idx ON processed_messages (processed_at);
CREATE INDEX decision_outbox_sent_idx ON decision_outbox (sent_at) WHERE sent_at IS NOT NULL;

-- +goose Down
DROP INDEX decision_outbox_sent_idx;
DROP INDEX processed_messages_processed_idx;
DROP INDEX account_events_ts_idx;
DROP INDEX decisions_decided_idx;
