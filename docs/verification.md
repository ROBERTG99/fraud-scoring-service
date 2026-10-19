# Verification

The latest verification run. It covers the gates that run in the build environment at the end of M6; the
spec §19 steps that need the M7 to M9 work are listed under [Not run](#not-run) and stay open (T-50).

| Item | Value |
|---|---|
| Date | 2026-10-19 |
| Commit | 2eba265 (branch `feat/friendly-johnson-15rki1`). 9164e26 and e993c9b, made after the run started, change documentation and the regenerated `evals/REPORT.md` only. |
| Environment | Claude Code on the web, Ubuntu 24.04.5, Linux 6.18.44 x86_64, 4 CPUs, 15 GiB ([`DEVELOPMENT.md`](DEVELOPMENT.md)) |
| Toolchains | go1.26.9, Python 3.13.16, uv 0.11.32, Node v22.22.0, pnpm 10.28.0, Docker 29.8.2 |
| Services | `docker compose -f compose.test.yaml -p fraudtest up -d --wait` (Postgres, Redis, Redpanda; 26 s) |
| API keys | none set |

## Gates

Each command ran once, in this order, from one script that recorded its exit code and wall-clock time.

| Command | Result | Time |
|---|---|---|
| `make lint` | **fail** at `lint-tf` only: `./scripts/terraform.sh: No such file or directory` (T-44). `lint-go`, `lint-py`, `lint-web`, `lint-proto` and `lint-md` (66 files, 0 issues) pass | 21 s |
| `make typecheck` | pass (mypy --strict on `src/` and `training/`, tsc --noEmit) | 21 s |
| `make check-fast` | pass | 88 s |
| `make test` | pass. Go: 22 packages ok with `-race -tags integration`, core coverage 86.4% (min 85), total 77.2% (min 70). Python: 665 passed, coverage 93.11% (min 70), `src/fraud_summarizer` 95.3% (min 85). Vitest: 67 passed in 11 files | 385 s |
| `make eval` | pass: Gate PASS, 5 of 5 checks; every suite's `evalgate check` against the baseline PASS; judge agreement 0.900, kappa 0.800; latency report within budget (p95 19.6 ms). Metrics as in [`evals/REPORT.md`](../evals/REPORT.md) | 112 s |
| `make security` | **fail**, closed: govulncheck NOT RUN (`Get "https://vuln.go.dev/index/modules.json.gz": Forbidden`, network policy). gitleaks, gosec, bandit, pip-audit, pnpm audit, hadolint, checkov, trivy (vulnerabilities and secrets on the scorer and case-summarizer dev images) and licences pass | 55 s |
| `make tf-check` | **fail**: `./scripts/tf_check.sh: No such file or directory` (T-44) | 0 s |

`make bench` was not re-run; the latency numbers are from the run of 2026-10-17 at f9ab2a5
([`PROGRESS.md`](../PROGRESS.md) T-39).

## Authorship check

```console
$ git log --format='%an <%ae> | %cn <%ce>' | sort -u
ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com> | ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com>
$ git log --format=%B | grep -cE 'Co-Authored-By|Claude-Session|Generated with Claude Code'
0
```

Run at e993c9b, the head before the commit that adds this file.

## Not run

These spec §19 steps need work that is not built yet, or a credential the environment does not have:

- **Step 1, `make bootstrap && make verify` in a fresh clone** (`make verify-clean`): `scripts/verify_clean.sh`
  does not exist yet, and `make verify` includes `test-docker`, `e2e` on the compose stack, `bench` and
  `tf-check`.
- **Step 2, compose stack, `make seed`, `make demo`, `make e2e`, `make test-docker`**: wait on the compose
  stack (T-43) and the demo script (T-46). The end-to-end suite last ran against the host stack (PROGRESS
  T-35).
- **Step 3, the `reviewer` over the whole history**: not run; milestone reviews ran per milestone (PROGRESS,
  milestone table).
- **Step 4, `make eval-live`**: no provider key; `evals/REPORT.md` says live evals were not run.
- **Step 7, moving `BUILD.md`**: left for the final verification (T-50).
- **govulncheck**: needs `vuln.go.dev` allowed in the environment's network policy.
