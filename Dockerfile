FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG APP_VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /out/flixer-updater ./cmd/flixer-updater

FROM alpine:3.22 AS runtime
ARG APP_VERSION=dev
# The Docker CLI/Compose plugin manage the stack; PostgreSQL client provides pg_dump and pg_restore.
RUN apk add --no-cache ca-certificates docker-cli docker-cli-compose postgresql17-client wget \
    && command -v pg_dump >/dev/null && command -v pg_restore >/dev/null \
    && mkdir -p /state

COPY --from=build /out/flixer-updater /usr/local/bin/flixer-updater

ENV APP_VERSION=${APP_VERSION} \
    PORT=8093 \
    STATE_DIR=/state

EXPOSE 8093

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=5 \
    CMD wget -qO- "http://127.0.0.1:${PORT}/health" >/dev/null || exit 1

# Runs as root: access to the Docker socket is what the updater is for.
ENTRYPOINT ["flixer-updater"]
