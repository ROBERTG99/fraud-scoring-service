-- SHA-256 of the Score request that produced the decision (security audit, D-044): a replay with the
-- same transaction_id but other contents is refused instead of answered. NULL for decisions written before.

-- +goose Up
ALTER TABLE decisions ADD COLUMN request_sha256 bytea NULL;

-- +goose Down
ALTER TABLE decisions DROP COLUMN request_sha256;
