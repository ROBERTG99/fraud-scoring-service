-- One role per service, least privilege (security audit M6 M1, D-043). Before this, every service used
-- fraud_app, which could UPDATE and DELETE decisions and analyst labels, so "the LLM never writes decisions"
-- was a code convention, not a privilege boundary. Development passwords only, as in 00001; deployments set
-- credentials from the environment. Roles are cluster-wide, so Down leaves them in place.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fraud_summarizer') THEN
    BEGIN
      CREATE ROLE fraud_summarizer LOGIN PASSWORD 'fraud_summarizer';
    EXCEPTION WHEN duplicate_object OR unique_violation THEN
      NULL; -- created at the same moment by another database's migration (roles are cluster-wide)
    END;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fraud_retention') THEN
    BEGIN
      CREATE ROLE fraud_retention LOGIN PASSWORD 'fraud_retention';
    EXCEPTION WHEN duplicate_object OR unique_violation THEN
      NULL; -- created at the same moment by another database's migration (roles are cluster-wide)
    END;
  END IF;
END
$$;
-- +goose StatementEnd

-- Scorer, outbox relay and console APIs: decisions are insert-only, labels are insert-only, nothing is deleted.
REVOKE ALL ON
  decisions, decision_outbox, review_cases, case_summaries, analyst_labels, shadow_comparisons,
  rule_sets, model_versions, step_records, processed_messages, account_events
  FROM fraud_app;
GRANT SELECT, INSERT ON decisions, analyst_labels, shadow_comparisons, account_events TO fraud_app;
GRANT SELECT, INSERT, UPDATE ON decision_outbox, review_cases, rule_sets TO fraud_app;
GRANT SELECT ON case_summaries, model_versions, step_records, processed_messages TO fraud_app;

-- Case summarizer: reads the case it summarizes, writes only its own tables.
GRANT SELECT ON decisions, review_cases, account_events TO fraud_summarizer;
GRANT SELECT, INSERT, UPDATE ON case_summaries TO fraud_summarizer;
GRANT SELECT, INSERT ON step_records, processed_messages TO fraud_summarizer;

-- Retention job: deletes expired rows (child rows go by ON DELETE CASCADE).
GRANT SELECT, DELETE ON decisions, decision_outbox, step_records, processed_messages, account_events
  TO fraud_retention;

-- +goose Down
REVOKE ALL ON
  decisions, decision_outbox, review_cases, case_summaries, analyst_labels, shadow_comparisons,
  rule_sets, model_versions, step_records, processed_messages, account_events
  FROM fraud_summarizer, fraud_retention;
GRANT SELECT, INSERT, UPDATE, DELETE ON
  decisions, decision_outbox, review_cases, case_summaries, analyst_labels, shadow_comparisons,
  rule_sets, model_versions, step_records, processed_messages, account_events
  TO fraud_app;
