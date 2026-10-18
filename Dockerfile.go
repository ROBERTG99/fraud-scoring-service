# syntax=docker/dockerfile:1.7
# One image target per Go binary: docker build -f Dockerfile.go --target scorer .
ARG GO_IMAGE=golang:1.26.9@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6
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
    go build -ldflags "-s -w -X github.com/ROBERTG99/fraud-scoring-service/internal/buildinfo.Version=${VERSION}" -o /out/scorer ./cmd/scorer
RUN mkdir -p /out/spool /out/audit

FROM ${RUNTIME_IMAGE} AS scorer
COPY --from=build /out/scorer /scorer
# The spool and the audit anchors are the only paths the scorer writes; compose mounts a volume here over a read-only root (D-055).
COPY --from=build --chown=65532:65532 /out/spool /var/lib/fraud/spool
COPY --from=build --chown=65532:65532 /out/audit /var/lib/fraud/audit
ENV FRAUD_SPOOL_DIR=/var/lib/fraud/spool
USER 65532:65532
EXPOSE 8510 8511 8516
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 CMD ["/scorer", "healthcheck"]
ENTRYPOINT ["/scorer"]
