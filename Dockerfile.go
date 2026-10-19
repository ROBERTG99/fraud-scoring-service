# syntax=docker/dockerfile:1.7
# One image target per Go binary: docker build -f Dockerfile.go --target scorer .
# Targets: scorer, feature-updater, dev-issuer, webserve (with the built console), retention.
ARG GO_IMAGE=golang:1.26.9@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6
ARG NODE_IMAGE=node:22.20.0-slim@sha256:b21fe589dfbe5cc39365d0544b9be3f1f33f55f3c86c87a76ff65a02f8f5848e
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# checkov:skip=CKV_DOCKER_7: the base image is a build arg whose default is a version tag, never latest
FROM ${GO_IMAGE} AS build
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG NO_PROXY
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    for b in scorer feature-updater dev-issuer webserve retention; do \
      go build -ldflags "-s -w -X github.com/ROBERTG99/fraud-scoring-service/internal/buildinfo.Version=${VERSION}" -o /out/$b ./cmd/$b || exit 1; \
    done
RUN mkdir -p /out/spool /out/audit

# The analyst console (React + Vite): static files served by webserve.
FROM ${NODE_IMAGE} AS web
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG NO_PROXY
WORKDIR /web
RUN npm install -g --no-fund --no-audit pnpm@10.28.0
COPY web/package.json web/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM ${RUNTIME_IMAGE} AS scorer
COPY --from=build /out/scorer /scorer
COPY rules /app/rules
COPY features/spec.yaml /app/features/spec.yaml
# The spool and the audit anchors are the only paths the scorer writes; compose mounts a volume here over a read-only root (D-055).
COPY --from=build --chown=65532:65532 /out/spool /var/lib/fraud/spool
COPY --from=build --chown=65532:65532 /out/audit /var/lib/fraud/audit
ENV FRAUD_SPOOL_DIR=/var/lib/fraud/spool FRAUD_RULES_DIR=/app/rules FRAUD_FEATURE_SPEC=/app/features/spec.yaml
USER 65532:65532
EXPOSE 8510 8511 8516
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 CMD ["/scorer", "healthcheck"]
ENTRYPOINT ["/scorer"]

FROM ${RUNTIME_IMAGE} AS feature-updater
COPY --from=build /out/feature-updater /feature-updater
COPY features/spec.yaml /app/features/spec.yaml
ENV FRAUD_FEATURE_SPEC=/app/features/spec.yaml FRAUD_UPDATER_HTTP_ADDR=:8512
USER 65532:65532
EXPOSE 8512
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 CMD ["/feature-updater", "healthcheck"]
ENTRYPOINT ["/feature-updater"]

FROM ${RUNTIME_IMAGE} AS dev-issuer
COPY --from=build /out/dev-issuer /dev-issuer
ENV FRAUD_ISSUER_ADDR=:8515
USER 65532:65532
EXPOSE 8515
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 CMD ["/dev-issuer", "healthcheck"]
ENTRYPOINT ["/dev-issuer"]

FROM ${RUNTIME_IMAGE} AS webserve
COPY --from=build /out/webserve /webserve
COPY --from=web /web/dist /app/web/dist
ENV FRAUD_WEB_DIST=/app/web/dist FRAUD_WEB_ADDR=:8500
USER 65532:65532
EXPOSE 8500
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 CMD ["/webserve", "healthcheck"]
ENTRYPOINT ["/webserve"]

# Batch job (`make retention`), not a server: compose tracks its exit code.
FROM ${RUNTIME_IMAGE} AS retention
COPY --from=build /out/retention /retention
USER 65532:65532
HEALTHCHECK NONE
ENTRYPOINT ["/retention"]
