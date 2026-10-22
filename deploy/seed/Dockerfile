# syntax=docker/dockerfile:1.7
# The `seed` job (`make seed`): migrate, generate the demo data (cmd/gen, seed 7), train and
# register `fraud-gbdt` in MLflow with the production and shadow aliases, then replay the last 72 h through
# the scorer (cmd/seed). Go binaries next to the training environment, since the job runs both.
ARG GO_IMAGE=golang:1.26.10@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82

# checkov:skip=CKV_DOCKER_7: the base image is a build arg whose default is a version tag, never latest
FROM ${GO_IMAGE} AS gobuild
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG NO_PROXY
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-s -w" -o /out/seed ./cmd/seed && go build -ldflags "-s -w" -o /out/gen ./cmd/gen

FROM python:3.14.7-slim@sha256:51dafde81dbdb6ebde285137a295cf18a47ca95234fe388a343719cb97305b3d AS pybuild
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG NO_PROXY
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
# Optional extra CA for TLS-intercepting build networks: --secret id=extra_ca,src=<bundle>. Never stored in a layer.
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export PIP_CERT=/run/secrets/extra_ca; fi; \
    pip install --no-cache-dir uv==0.11.3
WORKDIR /app
COPY pyproject.toml uv.lock README.md ./
RUN --mount=type=cache,target=/root/.cache/uv --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export SSL_CERT_FILE=/run/secrets/extra_ca UV_NATIVE_TLS=false; fi; \
    uv sync --frozen --no-default-groups --group training --no-install-project

FROM python:3.14.7-slim@sha256:51dafde81dbdb6ebde285137a295cf18a47ca95234fe388a343719cb97305b3d AS seed
RUN useradd --system --uid 10001 --create-home app && mkdir -p /data && chown 10001 /data
WORKDIR /app
COPY --from=pybuild /app/.venv /app/.venv
COPY --from=gobuild /out/seed /out/gen /usr/local/bin/
COPY training training
COPY features/spec.yaml features/spec.yaml
ENV PATH=/app/.venv/bin:$PATH PYTHONUNBUFFERED=1 PYTHONPATH=/app FRAUD_FEATURE_SPEC=/app/features/spec.yaml \
    FRAUD_SEED_DATA=/data/demo
USER 10001
# Batch job, not a server: compose tracks its exit code.
HEALTHCHECK NONE
ENTRYPOINT ["seed", \
  "-train-cmd", "python -m training.train --data /data/demo --out /data/model", \
  "-train-cmd", "python -m training.registry --model-dir /data/model --alias production", \
  "-train-cmd", "python -m training.registry --model-dir /data/model --alias shadow"]
