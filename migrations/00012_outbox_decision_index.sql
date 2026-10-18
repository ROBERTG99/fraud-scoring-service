-- Index the outbox's foreign key (M6 review, D-056): retention deletes decisions in batches and their outbox
-- rows cascade, which scanned decision_outbox once per deleted decision. Built CONCURRENTLY, outside a
-- transaction, so the scorer's inserts are not blocked while it builds.

-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS decision_outbox_decision_idx ON decision_outbox (decision_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS decision_outbox_decision_idx;
