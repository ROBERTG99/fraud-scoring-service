# syntax=docker/dockerfile:1.7
# One image target per Go binary: docker build -f Dockerfile.go --target scorer .
ARG GO_IMAGE=golang:1.24
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

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
    mkdir -p /out && for b in scorer; do \
      if [ -d "cmd/$b" ]; then go build -ldflags "-s -w -X github.com/ROBERTG99/fraud-scoring-service/internal/buildinfo.Version=${VERSION}" -o /out/$b ./cmd/$b; fi; \
    done

FROM ${RUNTIME_IMAGE} AS scorer
COPY --from=build /out/scorer /scorer
USER nonroot:nonroot
EXPOSE 8510 8511
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 CMD ["/scorer", "healthcheck"]
ENTRYPOINT ["/scorer"]
