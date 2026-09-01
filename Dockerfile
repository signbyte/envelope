ARG GO_VERSION=1.27.0

FROM golang:${GO_VERSION} AS build
WORKDIR /src

COPY . .

RUN go mod download
# VERSION is supplied by ci.yml (build-args) and reaches the binary through -X.
# Without both halves the pipeline computes a version that is thrown away and
# every log line reports the dev default instead of the build that is running.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/server ./cmd/server

FROM ghcr.io/wntrtech/scratch:v1.0.0-3
COPY --from=build /out/server /server

EXPOSE 8080/tcp
ENTRYPOINT ["/server", "web"]
HEALTHCHECK --start-period=20s --start-interval=5s --interval=1m --timeout=10s --retries=5 \
    CMD ["/server", "health"]
