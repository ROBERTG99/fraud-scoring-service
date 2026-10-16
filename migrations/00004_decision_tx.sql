-- Transaction attributes (amount, currency, mcc, channel, country, merchant) stored with each decision so
-- case detail and TestRule can rebuild the rule environment (docs/DECISIONS.md D-019).

-- +goose Up
ALTER TABLE decisions ADD COLUMN tx jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE decisions DROP COLUMN tx;
