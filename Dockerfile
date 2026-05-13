# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llmtap /usr/local/bin/llmtap
EXPOSE 4000
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/llmtap"]
CMD ["up"]
