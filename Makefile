# fraud-scoring-service — single entry point for build, test, eval and verification.
# Every target works offline with no API keys except eval-live and record-fixtures.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GOBIN ?= $(shell go env GOPATH 2>/dev/null)/bin
export PATH := $(GOBIN):$(HOME)/.local/bin:$(CURDIR)/.cache/bin:$(PATH)

GO ?= go
export GOTOOLCHAIN := local
# Root holds Dockerfile.go (spec §16), so Go package trees are listed explicitly.
GO_PKGS := ./cmd/... ./internal/... ./gen/... ./schemas/... ./features/... ./migrations/... ./tests/... ./scripts/gocover/...
UV ?= uv
PNPM ?= pnpm
COMPOSE ?= docker compose
COMPOSE_TEST ?= docker compose -f compose.test.yaml -p fraudtest
PY := $(UV) run --frozen

SIZE ?= demo
SEED ?= 7
ALIAS ?= production
DATA_DIR ?= artifacts/data/$(SIZE)
MODEL_DIR ?= artifacts/model
BENCH_PROFILE ?= ci
LIVE_EVAL_BUDGET_USD ?= 2

# Test infrastructure from compose.test.yaml (ports 8521/8531/8543).
TEST_ENV := FRAUD_TEST_PG_DSN=postgres://fraud:fraud@127.0.0.1:8521/fraud?sslmode=disable \
	FRAUD_TEST_REDIS_ADDR=127.0.0.1:8531 \
	FRAUD_TEST_KAFKA_BROKERS=127.0.0.1:8543

GO_CORE_PKGS := ./internal/features/... ./internal/rules/... ./internal/decide/... ./internal/store/...

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | sort | awk -F':.*## ' '{printf "  %-20s %s\n", $$1, $$2}'

# ---------------------------------------------------------------- setup
.PHONY: bootstrap
bootstrap: ## Install toolchains and dependencies, create .env from .env.example
	./scripts/bootstrap.sh
	@test -f .env || cp .env.example .env

# ---------------------------------------------------------------- stack
.PHONY: up down seed demo logs triton-up
up: ## Start the full stack with healthchecks
	$(COMPOSE) up -d --build --wait

down: ## Stop the full stack
	$(COMPOSE) down

seed: ## Migrate, generate demo data, train, register and replay 24 h
	$(COMPOSE) run --rm seed

demo: ## Run the scripted demo scenario against the running stack (offline)
	./scripts/demo.sh

logs: ## Follow logs of every service
	$(COMPOSE) logs -f --tail=100

triton-up: ## Start the optional Triton profile
	$(COMPOSE) --profile triton up -d --wait triton

# ---------------------------------------------------------------- quality
.PHONY: lint lint-go lint-py lint-web lint-proto lint-md lint-tf typecheck security check-fast
lint: lint-go lint-py lint-web lint-proto lint-md lint-tf ## gofmt, goimports, golangci-lint, ruff, eslint, prettier, buf, terraform fmt, markdownlint

lint-go:
	@out=$$(gofmt -l $$(git ls-files --cached --others --exclude-standard '*.go' | grep -v -e '^gen/' -e '^Dockerfile') || true); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@out=$$(goimports -l -local github.com/ROBERTG99/fraud-scoring-service $$(git ls-files --cached --others --exclude-standard '*.go' | grep -v -e '^gen/' -e '^Dockerfile') || true); if [ -n "$$out" ]; then echo "goimports needed:"; echo "$$out"; exit 1; fi
	golangci-lint run $(GO_PKGS)

lint-py:
	$(PY) ruff check .
	$(PY) ruff format --check .

lint-web:
	$(PNPM) -C web lint
	$(PNPM) -C web format:check
	$(PNPM) -C web exec prettier --check ../tests/e2e

lint-proto:
	buf lint
	buf format -d --exit-code

lint-md:
	$(PNPM) -C web exec markdownlint-cli2 --config ../.markdownlint-cli2.jsonc $(addprefix ../,$(shell git ls-files '*.md' | grep -v '^docs/specs/' | grep -v '^BUILD.md$$'))

lint-tf:
	./scripts/terraform.sh fmt -check -recursive infra/terraform

typecheck: ## mypy --strict on src/ and training/, tsc --noEmit
	$(PY) mypy --strict src training serving
	$(PNPM) -C web typecheck
	$(PNPM) -C web exec tsc -p ../tests/e2e

security: ## gitleaks, gosec, govulncheck, bandit, pip-audit, pnpm audit, hadolint, trivy, checkov, tflint, actionlint, licences
	./scripts/security.sh

check-fast: ## Lint and unit tests, under 60 seconds
	@out=$$(gofmt -l $$(git ls-files --cached --others --exclude-standard '*.go' | grep -v -e '^gen/' -e '^Dockerfile') || true); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(PY) ruff check .
	$(GO) vet $(GO_PKGS)
	$(GO) test -count=1 $(GO_PKGS)
	$(PY) pytest -q -p no:cacheprovider tests/unit tests/llm tests/harness -m "not slow"

# ---------------------------------------------------------------- tests
.PHONY: test test-go test-py test-web test-deps-up test-deps-down test-docker e2e coverage
test-deps-up:
	$(COMPOSE_TEST) up -d --wait postgres redis redpanda

test-deps-down:
	$(COMPOSE_TEST) down -v

test: test-deps-up ## Unit, integration and contract tests on the host
	$(MAKE) test-go test-py test-web

test-go:
	$(TEST_ENV) $(GO) test -race -count=1 -tags integration -coverprofile=artifacts/coverage/go.out -coverpkg=./internal/...,./cmd/... $(GO_PKGS)
	$(GO) run ./scripts/gocover -profile artifacts/coverage/go.out -core internal/features,internal/rules,internal/decide,internal/store -min-core 85 -min-total 70

test-py:
	$(TEST_ENV) $(PY) pytest -q tests/unit tests/llm tests/harness tests/integration tests/contract \
		--cov=src/fraud_summarizer --cov=training --cov-report=term-missing:skip-covered --cov-fail-under=70
	$(PY) python scripts/pycover.py --package src/fraud_summarizer --min 85

test-web:
	$(PNPM) -C web test

test-docker: ## The full test suite inside containers
	./scripts/test_docker.sh

e2e: ## Playwright end-to-end tests against the running stack
	NODE_PATH=$(CURDIR)/web/node_modules $(PNPM) -C web exec playwright test --config ../tests/e2e/playwright.config.ts

# ---------------------------------------------------------------- data, model
.PHONY: proto gen-data train register-model feature-parity bench-model sagemaker-dry-run
proto: ## buf lint, breaking, codegen, TS types
	./scripts/proto.sh

gen-data: ## Generate synthetic data (SIZE=demo|eval, seed 7)
	$(GO) run ./cmd/gen -size $(SIZE) -seed $(SEED) -out $(DATA_DIR)

train: ## Train, calibrate and export the model on demo data
	@test -f $(DATA_DIR)/events.jsonl || $(MAKE) gen-data SIZE=$(SIZE)
	$(PY) python -m training.train --data $(DATA_DIR) --out $(MODEL_DIR)

register-model: ## Register the trained model and set ALIAS=production|shadow
	$(PY) python -m training.registry --model-dir $(MODEL_DIR) --alias $(ALIAS)

feature-parity: ## Go online vs Python offline features on the parity fixture
	$(PY) python scripts/feature_parity.py

bench-model: ## Model server single-row latency (p95 < 8 ms)
	$(PY) python scripts/bench_model.py

sagemaker-dry-run: ## Print the CreateTrainingJob request
	env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN -u AWS_PROFILE $(PY) python -m training.sagemaker_launch --dry-run

# ---------------------------------------------------------------- evals and bench
# Regression gating against evals/reports/baseline/<suite>.json; evalgate fails when the baseline is missing.
baseline = --baseline evals/reports/baseline/$(1).json
.PHONY: eval eval-model eval-summary eval-decisions eval-judge eval-live bench record-fixtures
eval: eval-model eval-decisions eval-summary eval-judge ## Offline evals with thresholds, writes evals/REPORT.md
	$(GO) run ./cmd/benchreport -check-only -report evals/reports/latency.json -allow-missing
	$(PY) python scripts/build_report.py

eval-model:
	@test -f artifacts/data/eval/events.jsonl || $(MAKE) gen-data SIZE=eval
	$(PY) python -m training.evaluate --data artifacts/data/eval --out evals/reports/model.json
	$(PY) evalgate check evals/reports/model.json $(call baseline,model) --max-regression-pp 1.0

eval-decisions:
	PYTHONPATH=$(CURDIR) $(PY) evalgate run evals/suites/decisions.yaml --split test --out evals/reports/decisions.json
	$(PY) evalgate check evals/reports/decisions.json $(call baseline,decisions) --max-regression-pp 1.0

eval-summary:
	PYTHONPATH=$(CURDIR) FRAUD_SUMMARY_PROVIDER=fake $(PY) evalgate run evals/suites/summaries.yaml --split test --out evals/reports/summaries.json
	$(PY) evalgate check evals/reports/summaries.json $(call baseline,summaries) --max-regression-pp 1.0

eval-judge:
	PYTHONPATH=$(CURDIR) $(PY) python -m evals.graders.judge --split test

eval-live: ## Live evals with real models within LIVE_EVAL_BUDGET_USD
	LIVE_EVAL_BUDGET_USD=$(LIVE_EVAL_BUDGET_USD) $(PY) python scripts/eval_live.py

bench: ## Performance tests with budgets (BENCH_PROFILE=ci|laptop)
	./scripts/bench.sh $(BENCH_PROFILE)

record-fixtures: ## Record LLM cassettes (needs provider keys)
	$(PY) python scripts/record_fixtures.py

# ---------------------------------------------------------------- infra
.PHONY: tf-check
tf-check: ## terraform fmt, validate, test, tflint, checkov (no AWS credentials)
	./scripts/tf_check.sh

# ---------------------------------------------------------------- operations
.PHONY: audit-verify retention dlq-peek
audit-verify: ## Verify the audit-log hash chain
	$(COMPOSE) exec -T scorer /scorer audit-verify

retention: ## Apply retention policy
	$(COMPOSE) run --rm retention

dlq-peek: ## Show the latest DLQ messages
	$(COMPOSE) exec -T redpanda rpk topic consume events.dlq -n 10 -o -10

# ---------------------------------------------------------------- gates
.PHONY: verify verify-clean docs clean
verify: ## Everything except eval-live; the single gate
	$(MAKE) lint
	$(MAKE) typecheck
	$(MAKE) security
	$(MAKE) test
	$(MAKE) test-docker
	$(MAKE) e2e
	$(MAKE) eval
	$(MAKE) bench
	$(MAKE) tf-check

verify-clean: ## make verify in a fresh clone in a temporary folder
	./scripts/verify_clean.sh

docs: ## Regenerate diagrams, OpenAPI, frontend types and screenshots
	./scripts/proto.sh
	E2E_SCREENSHOTS=1 NODE_PATH=$(CURDIR)/web/node_modules $(PNPM) -C web exec playwright test --config ../tests/e2e/playwright.config.ts screenshots.spec.ts

clean: ## Remove containers, volumes, caches and build outputs
	-$(COMPOSE) --profile '*' down -v --remove-orphans
	-$(COMPOSE_TEST) down -v --remove-orphans
	rm -rf artifacts .cache mlruns spool web/dist coverage
