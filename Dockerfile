# syntax=docker/dockerfile:1.7
# Base images pinned by digest — tags are mutable and this image ships
# signed with SLSA provenance; Dependabot keeps the digests current. (D1)
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/colinedwardwood/llmtap/internal/buildinfo.Version=${VERSION} \
        -X github.com/colinedwardwood/llmtap/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/colinedwardwood/llmtap/internal/buildinfo.Date=${BUILD_DATE}" \
      -o /out/llmtap ./cmd/llmtap

FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# OCI labels drive GHCR repo linking and image provenance display.
LABEL org.opencontainers.image.title="llmtap" \
      org.opencontainers.image.source="https://github.com/colinedwardwood/llmtap" \
      org.opencontainers.image.description="OpenTelemetry tap for LLM API traffic" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=build /out/llmtap /usr/local/bin/llmtap
# 4000 = default plaintext-loopback listen; 4443 = documented TLS listen.
EXPOSE 4000 4443
# Distroless has no curl — the binary probes itself. Override the URL
# when listen is non-default (TLS :4443, custom bind).
ENV LLMTAP_HEALTHCHECK_URL=http://127.0.0.1:4000/healthz
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/llmtap", "healthcheck"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/llmtap"]
CMD ["up"]
