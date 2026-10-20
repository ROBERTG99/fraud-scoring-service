# Verification

The final verification of the build (spec §19, T-50), run on 2026-10-19 in two places: a fresh clone on a
laptop, and GitHub Actions on Linux x86-64, where every workflow checks out the branch fresh. No `.env` file and
no provider key was present in either.

| Item | Value |
|---|---|
| Date | 2026-10-19 |
| Commit | 271e373 for the fresh-clone run; `make test` and `make test-docker` re-run at 3f9a346 after the two fixes below. CI: 3f9a346 (all workflows) and e4a158b (running at the time of writing) |
| Laptop | macOS 27.0.1 arm64, 10 CPUs, Docker Desktop 29.8.2 (8 GB VM); the clone in a local temporary folder, not the synced working copy |
| Toolchains | go1.26.9, Python 3.12 (uv 0.11.8), Node v22.16.0, pnpm 10.28.0, Terraform 1.16.5, tflint 0.63.1 |
| CI | GitHub Actions `ubuntu-24.04` runners, [ROBERTG99/fraud-scoring-service#5](https://github.com/ROBERTG99/fraud-scoring-service/pull/5) |
| API keys | none set; live evals not run |

## Step 1: `make bootstrap && make verify` in a fresh clone

The laptop run cloned the commit into a temporary folder, ran `make bootstrap`, removed the `.env` it creates,
and ran each part of `make verify` (and the §19 step 2 commands) from one script that recorded exit codes and
wall-clock times.

| Command | Result | Time |
|---|---|---|
| `make bootstrap` | pass | 5 s |
| `docker compose up -d --build --wait` | pass: every service healthy | 51 s |
| `make seed` | pass: trained, registered `@production` and `@shadow`, 16,943 decisions, 270 degraded, 109 open cases | 97 s |
| `make demo` | pass: "demo: all 13 checkpoints OK" | 17 s |
| `make lint` | pass | 9 s |
| `make typecheck` | pass | 20 s |
| `make security` | pass on every scanner (gitleaks, gosec, govulncheck, bandit, pip-audit, pnpm audit, hadolint, checkov, trivy on nine images, tflint, actionlint, licences) | 40 s |
| `make test` | fail at 271e373: the wall-clock test of D-078 took 47.2 ms (budget 45) with every package under `-race`; replaced by deadline checks. **Pass at 3f9a346**: Go core coverage 86.9% (min 85), total 77.0% (min 70); Python 691 passed, coverage 92.96%, `src/fraud_summarizer` 95.3% (min 85); Vitest 67 passed | 269 s |
| `make test-docker` | fail at 271e373: `TestSpoolReplayAfterPostgresRecovers` saw one spooled commit take 121 ms (limit 60 ms, an fsync in the Docker VM). **Pass at 3f9a346** (691 Python tests, Go and Vitest in the container) | 328 s |
| `make e2e` | pass: 49 passed (Playwright, axe) | 60 s |
| `make eval` | **fail** at `eval-model`: the model trained on arm64 misses `band_calibration_gap` (0.0759, limit 0.05); every other threshold passes. CI on x86-64 reproduces the baseline model and passes (below; `docs/KNOWN_LIMITATIONS.md`) | 108 s |
| `make bench` | pass: 300 rps achieved, p50 7.8 ms, p95 19.6 ms (budget 80), p99 33.2 ms, success 1.0, degraded 0.0108 | 87 s |
| `make tf-check` | pass: validate (module and env), `terraform test` 2 passed, tflint clean, checkov 31 passed, 0 failed | 13 s |
| `docker compose down -v` | pass | 8 s |

On GitHub Actions the same parts run as separate jobs on every push, and the `verify` workflow runs the whole
target in one job:

| Workflow (3f9a346) | Result |
|---|---|
| `ci` (lint, typecheck, unit, integration with coverage gates and feature parity, bench at 300 RPS) | pass |
| `e2e` (`make up`, `make seed`, Playwright and axe, `make demo`) | pass |
| `evals` (decisions and summaries through `ROBERTG99/evalgate/action@v0.1.0`; `make eval` Gate PASS 4 of 4, `band_calibration_gap` 0.0081) | pass |
| `docker` (every image, `make security`, `make test-docker`) | pass |
| `terraform`, `codeql` | pass |
| `verify` (`make bootstrap && make verify` in one job) | **fail**: `TestRetriedBatchDoesNotDuplicateDLQ` counted DLQ records in a fixed 2 s window and saw none on the loaded runner; fixed in e4a158b (waits for the first record, then 2 s more for duplicates). The re-run on e4a158b had not finished when this was written (T-50 open) |

## Step 2: compose stack, seed, demo, e2e, test-docker

Covered by the rows above (laptop) and by the `e2e` and `docker` workflows (CI). From empty volumes the laptop's
`make up` plus `make seed` took 51 s + 97 s.

## Step 3: reviews

`reviewer` (whole branch since 5d877b7): **ship**, no blockers. `latency-reviewer`: ship. `ml-reviewer`: one
latent bug, fixed. `security-auditor`: no High findings; the three marked before-merge were fixed and the rest
are open findings in [`security.md`](security.md). Details in [`PROGRESS.md`](../PROGRESS.md) (T-49) and
[`DECISIONS.md`](DECISIONS.md) (D-077).

## Step 4: live evals

Not run: no `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` in the environment. [`evals/REPORT.md`](../evals/REPORT.md)
says so.

## Authorship

Both commands of spec §19 step 5, over the whole history at the commit above:

```text
$ git log --format='%an <%ae> | %cn <%ce>' | sort -u
ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com> | GitHub <noreply@github.com>
ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com> | ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com>
ROBERTG99 <robertino.gattas@gmail.com> | ROBERTG99 <robertino.gattas@gmail.com>
$ git log --format=%B | grep -E 'Co-Authored-By|Claude-Session|Generated with Claude Code'
```

Every author is the maintainer, under the address of the local git configuration or the GitHub no-reply address.
The committer `GitHub <noreply@github.com>` appears only on the merge commits of pull requests #1, #2 and #4,
which the maintainer merged in the GitHub web interface. The second command printed nothing.

## Not reproduced here

- `make eval` on arm64 (above). The shipped model is trained on x86-64.
- Triton (`make triton-up`) and the `observability` profile against the compose stack were not part of this run;
  T-41's evidence used the host stack (`docs/KNOWN_LIMITATIONS.md`).
