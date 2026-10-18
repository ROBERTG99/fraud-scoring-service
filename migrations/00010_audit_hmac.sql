-- Keyed audit chain (security audit M6 M9, D-050). New entries are HMAC-SHA256 over length-prefixed fields
-- (alg 2) under a key held outside the database (FRAUD_AUDIT_KEY), so database access alone cannot append an
-- entry that verifies. Earlier entries keep alg 1 (unkeyed sha256) and verify only before the first alg 2
-- entry. The append-only triggers of 00003 stop every role that does not own the table; the owner (the
-- migration role, which no service runs as) or a superuser can still disable them, which the keyed chain
-- and the anchors written outside the database (`scorer audit-anchor`) make detectable, not impossible.

-- +goose Up
ALTER TABLE audit_log ADD COLUMN alg smallint NOT NULL DEFAULT 1 CHECK (alg IN (1, 2));
ALTER TABLE audit_log ALTER COLUMN alg DROP DEFAULT;

-- +goose Down
ALTER TABLE audit_log DROP COLUMN alg;
