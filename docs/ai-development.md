# How this repository was built

This repository was built by a coding agent working from a written specification, inside a harness of
subagents, skills, hooks and rules that the agent wrote first. This page explains how the spec and the
harness were used, what the review loop found, and which mistakes were made along the way. Every fact here
comes from [`PROGRESS.md`](../PROGRESS.md), [`docs/DECISIONS.md`](DECISIONS.md), the specification
([`docs/specs/000-build-spec.md`](specs/000-build-spec.md)) or `git log`, and each one is cited where it
appears.

Status when this page was written (2026-10-19): milestones M0 to M6 are done and reviewed, and M7 to M9 have
not started (`PROGRESS.md`, Session handoff and Tasks). The one exception is the documentation set of T-47,
this page included, which the maintainer asked for ahead of M7 (D-070).

## The specification drove the build

The specification is one file, `BUILD.md`, copied to `docs/specs/000-build-spec.md` in M0 (spec §1.2). The
agent was started with one instruction: execute it end to end and do not stop until every item in the Global
Definition of Done is checked (spec header). Three parts of it shaped the work.

- **Operating mode (spec §0).** The agent works alone and nobody answers questions. It decides each ambiguous
  point and records the decision in `docs/DECISIONS.md` with alternatives and a reason (rule 2). Progress
  lives in `PROGRESS.md`, never in the spec (rule 3). Each task is a small verified increment: failing test,
  implementation, checks, evidence, one Conventional Commit (rule 4). Numbers are never invented and tests or
  thresholds are never weakened; a gap goes to `docs/KNOWN_LIMITATIONS.md` (rule 6). Everything runs offline
  with no API keys (rule 7).
- **Work plan (spec §8).** Fifty tasks, T-01 to T-50, in ten milestones, M0 to M9. Each task names its files,
  the test to write first, a "done when" condition and its dependencies. T-05 is a walking skeleton in Docker
  (a rules-only scorer and a summarizer on the fake LLM) built before any depth.
- **Definition of Done (spec §9).** Per task: the first test passes, the area checks pass, evidence is in
  `PROGRESS.md` and there is one commit. Per milestone: `make check-fast` is green, the `reviewer` subagent
  has no blockers and the branch is pushed. A Global Definition of Done (19 items) is mirrored in
  `PROGRESS.md` as a checklist; 3 of them are checked today (`PROGRESS.md`, Global Definition of Done).

`PROGRESS.md` is the agent's memory across sessions. Its "Session handoff" section holds a resume checklist
(environment setup, test infrastructure, pitfalls), the next steps and the open review items. CLAUDE.md
repeats the rule: after a context reset, re-read the spec, `PROGRESS.md`, `docs/DECISIONS.md` and
`git log --oneline -30`, then resume from the first unchecked task. Items only the maintainer can resolve
(a missing upstream tag, no LLM provider key, a network host the environment blocks) go under "Needs the
maintainer" at the top of `PROGRESS.md`.

During M0 to M6 the agent recorded D-001 to D-069 in `docs/DECISIONS.md`, dated 2026-10-16 to 2026-10-18.
Many are not ambiguities in the spec but corrections: the "Reason" column says which review found the
problem and which test now covers it. Architectural choices also get an ADR. The first one,
[`docs/adr/0001-feature-spec-v2-first-seen-device.md`](adr/0001-feature-spec-v2-first-seen-device.md), came
out of a mistake (D-026, mistake 2 below); the eight ADRs spec §18 asks for are numbered 0002 to 0009
(D-070).

At commit 2eba265 the history has 134 commits on branch `feat/friendly-johnson-15rki1`, all authored and committed by the
maintainer (`git log --oneline | wc -l` and `git log --format='%an <%ae> | %cn <%ce>' | sort -u`, run on
2026-10-19).

## The harness

The harness was built in M0 (T-03, T-04) and used from then on (spec §0 rule 9). CLAUDE.md summarises it; the
files are in `.claude/`.

### Subagents

All eight are defined in `.claude/agents/`. The reviewers are read-only: their tool lists contain Read, Grep,
Glob and Bash, and `reviewer` is told never to edit files.

| Subagent | Purpose | When it ran (recorded) |
|---|---|---|
| `reviewer` | Milestone review against the spec, CLAUDE.md and the rules; verdict "ship" or "fix first" | End of every milestone, M0 to M6, recorded as the code reviewer (`PROGRESS.md`, Milestone reviews) |
| `ml-reviewer` | Leakage, temporal split, calibration, train/serve parity | M2 (split, calibration, ONNX parity and D-008 judged sound) and M5 (2 blockers, D-037) |
| `ux-reviewer` | The §13 screens, four states per screen, WCAG 2.2 AA | M4: 7 blockers in round 1, 1 regression in round 2, then ship |
| `security-auditor` | STRIDE, OWASP Top 10 and OWASP LLM Top 10 | M6, in all nine rounds (D-041 to D-069) |
| `latency-reviewer` | The `Score` hot path: deadlines, pools, allocations | M6 round 1: tracing within budget, 3 allocation fixes (D-056) |
| `test-designer` | Failing-first tests for a task | No run is recorded in `PROGRESS.md` or `docs/DECISIONS.md` |
| `eval-analyst` | Reading eval reports against baselines | No run is recorded |
| `docs-editor` | Accuracy and cited numbers in the §18 documents | No run is recorded; the spec schedules it for T-47 (M8) |

Spec §0 rule 9 allows parallel subagents only for read-heavy work such as reviews and audits, never for two
agents editing the same files. The reviewers only report; the fixes are made afterwards on the main thread.
In M6 the code review and the security audit both looked at the same commit in each round (`PROGRESS.md`,
M6 row).

### Skills

Thirteen skills live in `.claude/skills/`, each a `SKILL.md` with numbered steps: `tdd`, `quality-gate`,
`eval-report`, `new-adr`, `add-migration`, `add-endpoint`, `add-llm-route`, `add-eval-case`, `demo-capture`,
`release`, and the domain skills `add-rule`, `train-and-register-model` and `add-fraud-pattern`. The `tdd`
skill is the per-task loop of spec §0 rule 4; `quality-gate` runs the gates in order and stops at the first
failure. `PROGRESS.md` records commands and results, not which skill was invoked, so this page does not claim
which skills ran in which task. [`CONTRIBUTING.md`](../CONTRIBUTING.md) points to the skills for common
changes.

### Hooks

Four hooks are registered in `.claude/settings.json` and tested by `tests/harness/test_hooks.py`
(`uv run pytest tests/harness/test_hooks.py` → 35 passed at T-04; 37 passed when rerun on 2026-10-19).

- **PreToolUse, `pre_tool_guard.py`.** Blocks what spec §0 rule 8 and §1.1 forbid: `terraform apply` and
  `destroy`, force pushes, `git config user.*`, setting `GIT_AUTHOR_*` or `GIT_COMMITTER_*`, attribution
  trailers, creating, deleting or changing the visibility of GitHub repositories, writes to `.env`, and edits
  to eval test partitions (`evals/datasets/**/test*.jsonl`) once they exist. It runs on shell, read and
  file-editing calls, uses only the standard library and never blocks on malformed input.
- **PostToolUse, `post_edit_format.sh`.** Formats the file just edited (goimports or gofmt, ruff, prettier,
  `terraform fmt`, `buf format`), skips `gen/`, and is a no-op when a formatter is missing.
- **Stop, `stop_check.sh`.** Runs `make check-fast` before the agent stops and feeds failures back; it honours
  `stop_hook_active`, so it cannot loop.
- **SessionStart, `session_start.sh`.** Cloud sessions only (`CLAUDE_CODE_REMOTE=true`): starts Docker with a
  registry mirror and runs `scripts/bootstrap.sh`; it is idempotent.

The guard shaped the work as well as blocking it. Because test partitions cannot be edited after they are
generated, the T-36 plan says "get the generator right before the first commit", and every decision case was
run through the real scorer before that commit (`PROGRESS.md`, T-36; D-031). The resume checklist also notes
two ways the guard and the shell got in the way: a command that mixes `rm -rf <path>` with unrelated paths is
blocked, and `pkill -f` on a pattern that matches the running shell kills it.

### Path-scoped rules

`.claude/rules/` holds twelve rules. `git-commits.md` has no frontmatter, so it always loads. The others load
when a matching path is touched:

- `go.md` (`**/*.go`, `go.mod`): a deadline on every I/O call, `-race` clean, no sleeps in tests.
- `python.md` (`**/*.py`, `pyproject.toml`): `uv run`, `mypy --strict`, Pydantic v2, pytest-socket.
- `frontend.md` (`web/**`): generated types, one TanStack Query layer, four states per screen.
- `proto.md` (`proto/**`, `buf*.yaml`): never renumber fields, `buf breaking`.
- `llm-code.md` (`src/fraud_summarizer/**`, `prompts/**`, `config/llm_routes.yaml`): spec Appendix B.
- `testing.md`, `evals.md`, `security.md`, `docker.md`, `terraform.md`, `docs.md` for their areas.

## Test first and the quality gates

Every task row in `PROGRESS.md` names the test written first and the command that showed it passing, for
example T-24: `go test -race -count=3 ./internal/api` with `TestModelTimeoutRulesOnlyWithin35ms`. Review
blockers follow the same rule: each fix comes with a regression test that fails without it, named in the
milestone row or the D-0xx entry (for example `TestRunReturnsWhileHandlerKeepsFailing` in M1, and the
M6 audit tests listed under T-42).

The gates are Make targets:

- `make check-fast`: gofmt, ruff, `go vet`, `go test` and the unit, LLM and harness pytest suites. The Stop
  hook runs it.
- `make test`: starts Postgres, Redis and Redpanda from `compose.test.yaml`, then runs the Go suite with
  `-race -tags integration` and coverage gates (85% on `internal/{features,rules,decide,store}`, 70% overall),
  the Python suites (85% on `src/fraud_summarizer`) and Vitest.
- `make eval`: the offline eval suites, gated by `evalgate check` against baselines; writes
  `evals/REPORT.md`.
- `make security`: every scanner, failing closed when one cannot run.
- `make verify`: `lint`, `typecheck`, `security`, `test`, `test-docker`, `e2e`, `eval`, `bench`, `tf-check`.

`make verify` does not pass yet. `make lint` stops at `lint-tf` until T-44 adds Terraform, the scripts behind
`make test-docker` and `make tf-check` arrive in M7, and `make security` fails closed because govulncheck
cannot reach `vuln.go.dev` from the build environment (`PROGRESS.md`, Session handoff and T-42).

## The review loop

### Milestone reviews

The `reviewer` returned "fix first" at least once on every milestone (`PROGRESS.md`, Milestone reviews):

- **M0:** `go vet ./...` parsed `Dockerfile.go`, and the `.env.*` deny rule also blocked `.env.example`.
- **M1:** the Kafka consumer could hang on shutdown while a handler kept failing, and the updater's Redis
  client timed out pipelines.
- **M2:** the provider schema dropped the `pattern` property, the faithfulness regex missed numbers with units
  such as `12x`, and invalid JSON was retried against Appendix B (D-014).
- **M3:** three rounds, covered under mistakes 1 and 5 below.
- **M4:** one code blocker and seven UX blockers in round 1; one UX regression in round 2; then ship.
- **M5:** two code blockers and two ML blockers in round 1 (mistakes 3 and 4); both reviewers said ship in
  round 2, after reproducing the numbers from clean `git archive` copies.

### The M6 security audit: nine rounds

M6 (T-41 telemetry, T-42 security controls) took nine review rounds. Each round ran the code reviewer and
the `security-auditor` on one commit; round 1 also ran the `latency-reviewer`. The table summarises the M6
row of `PROGRESS.md`.

| Round | Reviewed | Code review | Security audit | Fixed in | Decisions |
|---|---|---|---|---|---|
| 1 | T-41, T-42 | 2 blockers | 3 High, 9 Medium, 11 Low; 13 unbacked claims in `docs/security.md` | 90fb66b to 1d1b596 | D-041 to D-056 |
| 2 | 1d1b596 | 2 blockers | 1 High, fail-open Mediums | 16bdf75, cace120, 8990794, 05bdd3d | D-057 to D-060 |
| 3 | 0345f58 | 3 blockers | 4 Mediums, Lows | 8b1bf2e and the two commits after it | D-061 to D-063 |
| 4 | acf152c | 7 blockers | 2 Mediums, 4 Lows | a95a38c | D-064 |
| 5 | e92eaa1 | 1 blocker | ship, with Lows | a259f58 | D-065 |
| 6 | 1b7c746 | 2 blockers | 1 Medium, Lows | 89c5420 | D-066 |
| 7 | b7c0676 | 2 blockers | 1 Medium, Lows | 1180ae3 | D-067 |
| 8 | 817930a | 2 blockers | 1 Medium, Lows | 8ee4d95 | D-068 |
| 9 | 8758c8b | ship | Lows only | 2929005 | D-069 |

Round 1 was broad: the High findings were unbounded rule expressions (H1), an unbounded `event_ts` that let a
caller reset or backdate an account's features (H2, D-041) and adjacent PII that slipped past the redactor
(H3). From round 4 on, most blockers were in two places: the redactor (`src/fraud_summarizer/redact.py`) and
the faithfulness check's screen for case-clearing language. Each fix to one of them opened a new bypass that
the next round found (mistake 6).

From round 2 on, each round's fixes were recorded with `make test` exit 0 and `make eval` PASS; rounds 2 to 8
add `make security` (all scanners pass except govulncheck, which cannot run here), rounds 4 to 9 add
`make check-fast` and the lint targets, and rounds 4 to 8 add `scripts/redact_parity.py` against the reviewed
commit, which reported 0 of 6,488 unique eval-dataset values changed each time (`PROGRESS.md`, M6 row). The
round-9 fixes (2929005) have not been reviewed: the maintainer closed M6 without a round 10.

## Mistakes

These are mistakes the agent made, how each was caught and what changed. They are taken from `PROGRESS.md`,
`docs/DECISIONS.md` and the commit messages.

### 1. A replay that leaked later events into features

- **What happened.** The seed job loaded the whole replay window's device and login events before scoring.
  Later sightings overwrote earlier ones in Redis (`ZADD GT`), so 8,970 of 16,863 decisions saw
  `new_device=1`, against 4 point-in-time (D-025, gen v1 data). The T-31 evidence of 144 and 148 open cases
  came from that leak (D-024).
- **How it was caught.** The M3 milestone review, round 1 (`PROGRESS.md`, M3 row).
- **What changed.** Replay applies each account's live events in event order on the same worker that scores
  that account (6868bcd). `TestReplayDoesNotLeakLaterEventsIntoFeatures` guards it, and after the fix 4 of
  16,863 decisions had `new_device=1`, the same 4 the offline features give (D-025). The leaked numbers were
  withdrawn in T-31 and D-024, not quietly replaced. Round 2 found the new test flaky on a cold Redis; it now
  warms up and fails on degraded decisions (M3 row).

### 2. Synthetic data that was too easy

- **What happened.** With the leak fixed, the seed produced 10 open cases against the T-31 gate of at least
  50 (D-025). Generator v1 separated fraud almost perfectly: 151 of 174 frauds scored at least 0.85, and
  `new_device` was almost always 0 by construction (D-026).
- **How it was caught.** The gap appeared when the corrected replay was measured (D-025) and went to the
  maintainer.
- **What changed.** The maintainer chose realistic data over a lower gate or threshold (D-026). Generator v2
  adds legitimate look-alikes and low-and-slow fraud, and `new_device` became "first seen less than 24 h
  ago" (ADR 0001). Data, model and every dependent number were regenerated; the T-31 rerun gave 74 open cases
  (`PROGRESS.md`, T-31).

### 3. A model threshold fit on the test split

- **What happened.** The gated combined recall@1%FPR, 0.9232, used a threshold chosen on the test split, and
  nothing measured the threshold the rule set actually ships (0.30). ECE was the only calibration gate, and
  ECE stays under 0.02 even for an all-zero predictor at a 1% base rate (D-037).
- **How it was caught.** The `ml-reviewer` in the M5 review, round 1 (`PROGRESS.md`, M5 row).
- **What changed.** The model suite fits no threshold on test (02aa3b3): combined recall at the
  valid-chosen threshold is 0.9161, recall at the shipped review threshold is 0.8387 at FPR 0.0032, and a band
  calibration gap is gated (D-037; `PROGRESS.md`, T-37). The 0.9232 figure is still reported, labelled as the
  test oracle.

### 4. Gates that passed when something was missing

- **What happened.** In M5, a missing baseline silently dropped `--baseline`, a missing suite counted as "not
  run" instead of a failure, and the judge calibration output sat inside the report glob, so `make check-fast`
  went red after `make eval` (6bb26f9). In M6, `scripts/security.sh` passed when `uv export` failed (pip-audit
  audited an empty file) or when no images were listed (no trivy scan ran) (31872fe).
- **How it was caught.** The M5 code review, round 1, and the M6 code review, round 1 (`PROGRESS.md`, M5 and
  M6 rows).
- **What changed.** The gates fail closed: `test_missing_baseline_fails_the_gate`,
  `test_missing_required_suite_fails_the_gate`, and a NOT RUN row that fails `make security`.
  `tests/harness/test_security_script.py` runs the script with stub tools and fails against the round-1
  version (D-060). This is why govulncheck, which cannot run here, keeps `make security` red.

### 5. A breaker that never closed

- **What happened.** A hung Postgres turned 1,896 of 3,200 Score responses into RESOURCE_EXHAUSTED in
  `TestHungPostgresSpoolsInsteadOfOverloading` (D-027). The first fix added a breaker that spooled to disk,
  but it closed only when the spool was empty, which never happens under continuous traffic.
- **How it was caught.** The M3 review, round 1 for the overload and round 2 for the breaker
  (`PROGRESS.md`, M3 row).
- **What changed.** The breaker closes on a successful ping and the backlog drains in the background (D-027,
  2cec37e), covered by `TestBreakerClosesUnderContinuousTraffic`. In round 3 the reviewer reran its own probe
  and saw the breaker close in 21–530 ms (M3 row).

### 6. Redactor fixes that introduced new leaks

- **What happened.** From M6 round 4 to round 9, each redactor fix opened a bypass that the next round
  found:
  - Round 4 stopped decoding `%40`, so in round 5 `pay%404111111111111111` reached the provider whole
    (D-064, D-065).
  - Round 5 decoded escapes, so in round 6 `&lt;/case_facts&gt;` became a real closing tag at the gateway and
    reopened the facts fence. Its reading of groups after "%" was exponential: 131 s for one 120-character
    merchant name (D-066).
  - Round 6 decoded only to a whitelist, which hid escaped separators: in round 7, 28 of 34 encodings leaked a
    card (D-067).
  - Round 7 decoded once and redacted in one pass: in round 8 a card was left in the gap beside a label, and
    38 of 38 double-encoded separators left every group readable (D-068).
  - Round 8 capped decoding at 8 rounds, so in round 9 a card digit stayed behind an escape nested 9 deep
    (D-069).
- **How it was caught.** The code reviewer and the `security-auditor`, each round, with fuzzed and hand-built
  inputs (D-064 to D-069).
- **What changed.** Every finding became a failing test first; the round-9 tests include
  `test_escapes_that_folding_rebuilds_are_decoded_to_the_end` and
  `test_names_that_only_start_with_an_entity_do_not_grow` (D-069). The redactor now decodes until the text
  stops shrinking, repeats redaction until nothing changes and fails closed when either step does not
  converge (D-068, D-069). `scripts/redact_parity.py` checks that each fix leaves ordinary eval data alone (0
  of 6,488 unique values changed in rounds 4 to 8, `PROGRESS.md`, M6 row). The remaining gaps are written
  down in `docs/KNOWN_LIMITATIONS.md` ("Redaction gaps that remain") and `docs/security.md` (open findings 8
  and 9).

### 7. A latency budget broken by a security fix

- **What happened.** The offline summary p95 budget is 200 ms (spec §11). The first round-7 version ran
  redaction through `asyncio.to_thread` with no cache and failed it at 227.6 ms (D-067). The first round-8
  version always repeated redaction and failed it at 207.5 ms (D-068).
- **How it was caught.** `make eval`, run before pushing each round.
- **What changed.** Round 7 moved redaction to a long-lived two-thread pool with a cache (p95 139.1 ms).
  Round 8 stops the repeat at once when nothing was labelled, skips the decode bookkeeping for text without
  escapes and counts digits with one regex (p95 55.9 ms). Neither failing version was
  pushed (D-067, D-068; `PROGRESS.md`, M6 row). The cost remains: offline summary p50 rose from 6.3 ms before
  round 4 to 31.5 ms at round 9 (`PROGRESS.md`, M6 row; `evals/REPORT.md`).

### 8. Tests broken by a change, and tests not run

- **What happened.** Bounding `event_ts` to the last 24 hours (bf3d2a6, D-041) broke two tests that send
  historical timestamps: the OpenAPI contract test (fixed in 6e24096, four commits later) and the
  integration test `TestSeedIdempotent` (fixed in 2682ab3, thirteen commits later). Until those fixes the
  contract and integration suites could not pass, against the rule "never leave the branch red". Separately, a gofmt failure once silently
  skipped the tests because the test run was chained after a lint step with `&&` (`PROGRESS.md`, Resume
  checklist).
- **How it was caught.** The commit messages of 6e24096 and 2682ab3 record the fixes but not how the
  failures were noticed.
- **What changed.** The seed test now runs with the bound lifted, as the seed stack does
  (`MaxEventAge: -1`, D-041), so the fix configures the test like production replay rather than removing the
  check. The resume checklist now says to use `set -o pipefail` and never chain the test run after a lint
  step. A related case shows a full run catching a change: the first round-2 version of the duplicate check
  re-read the request hash in a second query, and `TestSeedIdempotent` timed out under load (D-057).

### 9. Security claims the code did not back

- **What happened.** The first `docs/security.md` contained 13 claims the code did not back; after round 2,
  12 more claims needed correcting. A code comment had the same problem: migration 00003 said its triggers reject
  changes "for every role, including owners", but the owner can disable the triggers (D-050).
- **How it was caught.** The `security-auditor` checked the document against the code in rounds 1 and 2
  (`PROGRESS.md`, M6 row; D-060).
- **What changed.** `docs/security.md` was rewritten against the code, with the accepted and open findings
  listed (`PROGRESS.md`, T-42), and migration 00010 corrects the comment (D-050).

### 10. A benchmark that reported success while the scorer degraded

- **What happened.** A first laptop-profile run of `make bench` reported success 1.0 while 97% of decisions
  were degraded: fast fallbacks were being measured as the scorer's latency (D-034).
- **How it was caught.** D-034 records the run but not who noticed it.
- **What changed.** `cmd/benchreport` counts degraded decisions in Postgres for the measured window and fails
  over 5% degraded and under 95% of the target rate (D-034). The laptop profile is reported as failing in the 4-vCPU container (586 rps achieved of 2,000, p95
  603.8 ms), not hidden (`PROGRESS.md`, T-39).

### 11. A summarizer that died silently

- **What happened.** No REVIEW case ever got a summary. The Kafka producers compress with snappy, the
  Python consumer had no codec and died on its first fetch, and `/healthz` stayed 200. Without a provider key,
  the SDK's TypeError also made the consumer retry one message forever (1b5feb0).
- **How it was caught.** The T-35 end-to-end run on the full host stack; unit and integration tests had used
  uncompressed batches (`PROGRESS.md`, T-35).
- **What changed.** The consumer installs the codecs and the integration test produces snappy batches; a
  crashed background task is logged and turns `/readyz` to 503, which the image healthcheck probes; an
  unexpected provider exception becomes a non-retryable error (1b5feb0).

### 12. Console amounts shown 100 times too large

- **What happened.** The timeline showed `amount_minor` as major units, so amounts were 100 times too large
  (3b48a6e).
- **How it was caught.** The `ux-reviewer` in the M4 review, round 1 (`PROGRESS.md`, M4 row).
- **What changed.** Amounts are formatted from minor units, with failing-first Vitest tests and a server
  test, `TestListGetTimelineShadowAndRules` (M4 row). The screenshot `docs/images/timeline.png` still shows
  the bug and is listed for recapture (`PROGRESS.md`, Global Definition of Done).

## What worked and what did not

### What worked

- **Evidence in the repository.** Every task row carries a command and its result, so reviewers could
  reproduce numbers (M3 round 3, M5 round 2) and wrong numbers could be withdrawn openly (T-31, D-024).
- **Failing-first regression tests for every review finding.** Each D-0xx correction names the test that
  failed before the fix, so a bug found once stays fixed.
- **Read-only reviewers.** Each milestone review found blockers the author had missed (`PROGRESS.md`,
  Milestone reviews); the specialised reviewers found what a general review did not (leakage in M3,
  test-split thresholds in M5, contrast and focus in M4).
- **Fail-closed gates.** Missing baselines, suites and scanners now fail (mistake 4), so a green gate means
  something ran.
- **Guard hooks.** The PreToolUse guard enforces the spec's safety rails and the commit attribution rule
  mechanically, and its 37 tests pass (`uv run pytest tests/harness/test_hooks.py`, 2026-10-19).

### What did not work

- **The redactor loop.** Nine rounds were spent mostly on one module. Fixes were local patches to the
  previous round's finding, and four of them opened the next round's bypass (mistake 6). A design that
  decodes completely and redacts to a fixpoint (rounds 8 and 9) would have been cheaper to reach first. The
  loop ended by the maintainer's decision, not by a clean review of the last fix (`PROGRESS.md`, M6 row).
- **Cost of the fixes.** The redactor work raised offline summary p50 from 6.3 ms to 31.5 ms and broke the
  latency budget twice before pushing (mistake 7).
- **Not running the full suite after a cross-cutting change.** The `event_ts` bound left integration and
  contract tests red for several commits (mistake 8).
- **Environment limits.** The build environment blocked `vuln.go.dev`, `nvcr.io` and the LLM providers, so
  govulncheck, the Triton profile and live evals have not run (`PROGRESS.md`, Needs the maintainer;
  `docs/KNOWN_LIMITATIONS.md`).
- **Unused subagents.** `test-designer`, `eval-analyst` and `docs-editor` have no recorded run so far.
- **Flaky timing under load.** One round-7 `make test` run failed `TestReplayDoesNotLeakLaterEventsIntoFeatures`
  on the 8 ms Redis budget on a loaded host; it is listed as an open non-blocking item since M3 (`PROGRESS.md`,
  M3 and M6 rows).

## Authorship

Commits and pull requests go out under the maintainer's name only. `.claude/settings.json` sets empty commit
and pull-request attribution, `.claude/rules/git-commits.md` forbids attribution trailers and identity
changes, and the PreToolUse guard blocks both. The evidence from the first commit is in `PROGRESS.md`
(Attribution evidence), and the full-history check of spec §19 step 5 is part of T-50.
