# Throwaway CI fixture — tag-pinned bases are fine here; this image is
# never published.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
ENV CGO_ENABLED=0
RUN go build -o /out/stub .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/stub /usr/local/bin/stub
EXPOSE 9999
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/stub"]
