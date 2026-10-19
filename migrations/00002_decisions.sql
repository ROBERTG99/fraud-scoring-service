-- +goose Up
CREATE TABLE decisions (
  decision_id    uuid PRIMARY KEY,
  transaction_id text NOT NULL UNIQUE,
  account_id     text NOT NULL,
  action         text NOT NULL CHECK (action IN ('APPROVE', 'REVIEW', 'DECLINE')),
  score          real NULL,
  reason_codes   text[] NOT NULL,
  model_version  text NOT NULL,
  rules_version  text NOT NULL,
  degraded       boolean NOT NULL,
  features       jsonb NOT NULL,
  latency_ms     real NOT NULL,
  decided_at     timestamptz NOT NULL
);
CREATE INDEX decisions_account_decided_idx ON decisions (account_id, decided_at DESC);
CREATE INDEX decisions_review_decided_idx ON decisions (decided_at) WHERE action = 'REVIEW';

CREATE TABLE decision_outbox (
  id          bigserial PRIMARY KEY,
  decision_id uuid NOT NULL REFERENCES decisions (decision_id) ON DELETE CASCADE,
  payload     jsonb NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  sent_at     timestamptz NULL,
  attempts    integer NOT NULL DEFAULT 0
);
CREATE INDEX decision_outbox_unsent_idx ON decision_outbox (id) WHERE sent_at IS NULL;

CREATE TABLE review_cases (
  case_id     uuid PRIMARY KEY,
  decision_id uuid NOT NULL UNIQUE REFERENCES decisions (decision_id) ON DELETE CASCADE,
  account_id  text NOT NULL,
  status      text NOT NULL CHECK (status IN ('open', 'labelled')),
  priority    real NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  labelled_at timestamptz NULL
);
CREATE INDEX review_cases_status_priority_idx ON review_cases (status, priority DESC);

CREATE TABLE case_summaries (
  case_id        uuid PRIMARY KEY REFERENCES review_cases (case_id) ON DELETE CASCADE,
  status         text NOT NULL CHECK (status IN ('ok', 'fallback', 'unfaithful')),
  summary        jsonb NOT NULL,
  pattern        text NOT NULL,
  faithfulness   real NOT NULL,
  model          text NOT NULL,
  prompt_version text NOT NULL,
  cost_usd       numeric(10, 6) NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX case_summaries_pattern_idx ON case_summaries (pattern);

CREATE TABLE analyst_labels (
  label_id        uuid PRIMARY KEY,
  case_id         uuid NOT NULL UNIQUE REFERENCES review_cases (case_id) ON DELETE CASCADE,
  label           text NOT NULL CHECK (label IN ('fraud', 'legit')),
  note            text NULL CHECK (char_length(note) <= 2000),
  analyst_sub     text NOT NULL,
  idempotency_key text NOT NULL UNIQUE,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE shadow_comparisons (
  decision_id     uuid PRIMARY KEY REFERENCES decisions (decision_id) ON DELETE CASCADE,
  primary_version text NOT NULL,
  shadow_version  text NOT NULL,
  primary_score   real NOT NULL,
  shadow_score    real NOT NULL,
  primary_band    text NOT NULL,
  shadow_band     text NOT NULL,
  agree           boolean NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX shadow_comparisons_created_idx ON shadow_comparisons (created_at DESC);

CREATE TABLE rule_sets (
  version   text PRIMARY KEY,
  sha256    text NOT NULL,
  body      jsonb NOT NULL,
  loaded_at timestamptz NOT NULL DEFAULT now(),
  active    boolean NOT NULL
);
CREATE UNIQUE INDEX rule_sets_one_active_idx ON rule_sets (active) WHERE active;

CREATE TABLE model_versions (
  version       text PRIMARY KEY,
  alias         text NOT NULL,
  backend       text NOT NULL,
  onnx_sha256   text NOT NULL,
  metrics       jsonb NOT NULL,
  registered_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX model_versions_alias_idx ON model_versions (alias);

CREATE TABLE step_records (
  run_id         text NOT NULL,
  task_id        text NULL,
  step           integer NOT NULL,
  node           text NOT NULL,
  kind           text NOT NULL,
  model          text NULL,
  prompt_version text NULL,
  input_tokens   integer NOT NULL,
  output_tokens  integer NOT NULL,
  cost_usd       numeric(10, 6) NOT NULL,
  latency_ms     real NOT NULL,
  status         text NOT NULL,
  error          text NULL,
  started_at     timestamptz NOT NULL,
  PRIMARY KEY (run_id, step)
);
CREATE INDEX step_records_started_idx ON step_records (started_at DESC);

CREATE TABLE processed_messages (
  consumer     text NOT NULL,
  message_key  text NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, message_key)
);

-- docs/DECISIONS.md D-005: account activity for timelines and summary facts.
CREATE TABLE account_events (
  event_id   text PRIMARY KEY,
  account_id text NOT NULL,
  kind       text NOT NULL CHECK (kind IN ('login', 'device', 'transaction')),
  event_ts   timestamptz NOT NULL,
  payload    jsonb NOT NULL
);
CREATE INDEX account_events_account_ts_idx ON account_events (account_id, event_ts DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON
  decisions, decision_outbox, review_cases, case_summaries, analyst_labels, shadow_comparisons,
  rule_sets, model_versions, step_records, processed_messages, account_events
  TO fraud_app;
GRANT USAGE, SELECT ON SEQUENCE decision_outbox_id_seq TO fraud_app;
GRANT SELECT ON
  decisions, decision_outbox, review_cases, case_summaries, analyst_labels, shadow_comparisons,
  rule_sets, model_versions, step_records, processed_messages, account_events
  TO fraud_readonly;

-- +goose Down
DROP TABLE account_events;
DROP TABLE processed_messages;
DROP TABLE step_records;
DROP TABLE model_versions;
DROP TABLE rule_sets;
DROP TABLE shadow_comparisons;
DROP TABLE analyst_labels;
DROP TABLE case_summaries;
DROP TABLE review_cases;
DROP TABLE decision_outbox;
DROP TABLE decisions;
