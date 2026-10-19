-- Audit sequence numbers start at 1 (M6 review round 3, D-061): an unkeyed rewrite placed at seq <= 0 would
-- otherwise sit under FRAUD_AUDIT_LEGACY_MAX_SEQ's default of 0. The table owner can drop this check, so
-- `scorer audit-verify` refuses such rows as well.

-- +goose Up
ALTER TABLE audit_log ADD CONSTRAINT audit_log_seq_positive CHECK (seq > 0);

-- +goose Down
ALTER TABLE audit_log DROP CONSTRAINT audit_log_seq_positive;
