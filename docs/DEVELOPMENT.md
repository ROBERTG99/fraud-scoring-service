# Development environment

## Reference build environment

Recorded on 2026-10-16; the measurements in this repository's docs come from this environment unless noted.

| Item | Value |
|---|---|
| Mode | Cloud development container |
| OS | Ubuntu 24.04.5 LTS, Linux 6.18.44 x86_64 |
| CPUs | 4 |
| Memory | 15 GiB |
| go | go1.24.7 linux/amd64 preinstalled; builds use go1.26.10 from go.mod's `toolchain` line (`GOTOOLCHAIN=auto`, D-042) |
| python3 | 3.13.16 |
| uv | 0.11.32 |
| node | v22.22.0 |
| pnpm | 10.28.0 |
| docker | 29.8.2 (daemon started by hand, registry mirror `mirror.gcr.io`) |
| buf | missing at start (installed with `go install`) |
| terraform | missing at start (installed from releases.hashicorp.com) |

## Network notes

- `docker.redpanda.com` is blocked; `redpandadata/redpanda` pulls through the `mirror.gcr.io` registry mirror.
- `buf.build` (BSR) is unreachable, so `google/api/*.proto` are vendored under `proto/third_party/` instead of a BSR dependency.
- `registry.terraform.io` is unreachable; the AWS provider is installed from releases.hashicorp.com into the implicit local mirror.
