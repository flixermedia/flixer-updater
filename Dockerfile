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
# The docker CLI and compose plugin drive the host daemon through the mounted socket; pg_dump takes the backups.
RUN apk add --no-cache ca-certificates docker-cli docker-cli-compose postgresql16-client wget \
    && mkdir -p /state /backups

COPY --from=build /out/flixer-updater /usr/local/bin/flixer-updater

ENV APP_VERSION=${APP_VERSION} \
    PORT=8093 \
    STATE_DIR=/state \
    BACKUP_DIR=/backups

EXPOSE 8093

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=5 \
    CMD wget -qO- "http://127.0.0.1:${PORT}/health" >/dev/null || exit 1

# Runs as root: access to the Docker socket is what the updater is for.
ENTRYPOINT ["flixer-updater"]
