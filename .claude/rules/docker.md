---
paths:
  - "Dockerfile*"
  - "**/Dockerfile"
  - "compose*.yaml"
---
# Docker rules

- Multi-stage, non-root, HEALTHCHECK, `ARG HTTPS_PROXY` in every stage that downloads, no `apt-get`.
- Go images: static binary on `gcr.io/distroless/static`. Python images: `python:3.12-slim` + uv.
- Host ports 8500–8599 only; `depends_on` with `condition: service_healthy`.
- Containers are offline at runtime.
