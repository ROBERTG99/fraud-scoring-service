-- W3C traceparent of the Score span that produced the decision; the outbox relay sends it as the Kafka
-- `traceparent` header so the case summarizer continues the same trace. NULL when not traced.

-- +goose Up
ALTER TABLE decision_outbox ADD COLUMN traceparent text NULL;

-- +goose Down
ALTER TABLE decision_outbox DROP COLUMN traceparent;
