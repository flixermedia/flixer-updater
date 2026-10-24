# flixer-updater

A small sidecar that updates a Flixer Docker Compose stack on request from the Flixer admin dashboard.

For each update it:

1. takes a `pg_dump` backup (a failed backup aborts the update with nothing changed),
2. pins the requested release of each component in `/state/versions.env`,
3. pulls the new images,
4. recreates the services one at a time (`streamer`, `indexer`, `web`, then `api`) and waits for each to be healthy,
5. rolls back to the previous releases if a service fails to start or become healthy,
6. restarts the reverse proxy so it picks up the new containers.

Progress, the backup name and any error are reported through `GET /v1/status`.

## API

All `/v1` routes need `Authorization: Bearer <UPDATER_TOKEN>`.

| Route            | Description                                                                 |
| ---------------- | --------------------------------------------------------------------------- |
| `GET /health`    | Unauthenticated liveness check; returns the updater version.                |
| `GET /v1/status` | Pinned versions and the latest run (`status`, `step`, `log`, `error`, ...). |
| `POST /v1/update` | `{"targets":{"web":"0.9.0"},"current":{"web":"0.8.0"}}`. `202` when started, `409` if an update is running, `400` if invalid. |

Components: `flixer` (service `api`), `web`, `streamer`, `indexer`. Versions must be `X.Y.Z` releases.
`current` is used as the rollback target the first time a component is updated.

## Configuration

| Variable               | Default      | Notes                                                              |
| ---------------------- | ------------ | ------------------------------------------------------------------ |
| `UPDATER_TOKEN`        | required     | At least 16 characters; shared with the Flixer API.                |
| `PORT`                 | `8093`       |                                                                    |
| `DATABASE_URL`         | unset        | `postgres://user:pass@host/db`. Without it, no backup is taken.    |
| `BACKUP_DIR`           | `/backups`   | Last 5 dumps are kept.                                             |
| `STATE_DIR`            | `/state`     | `state.json` and `versions.env`.                                   |
| `HEALTH_TIMEOUT`       | `3m`         | Per service, minimum `10s`.                                        |
| `RESTART_SERVICES`     | `proxy`      | Comma-separated services restarted after an update.                |
| `COMPOSE_PROJECT_NAME` / `COMPOSE_PROJECT_DIR` / `COMPOSE_FILE` | auto | Detected from the updater's own container labels. |

## Compose setup

The project folder must be mounted at the **same absolute path** inside the container as on the host, and the compose
images must read their tag from the version variables:

```yaml
services:
  api:
    image: ghcr.io/flixermedia/flixer:${FLIXER_VERSION:-latest}
  web:
    image: ghcr.io/flixermedia/flixer-web:${WEB_VERSION:-latest}
  streamer:
    image: ghcr.io/flixermedia/flixer-streamer:${STREAMER_VERSION:-latest}
  indexer:
    image: ghcr.io/flixermedia/flixer-indexer:${INDEXER_VERSION:-latest}

  updater:
    image: ghcr.io/flixermedia/flixer-updater:latest
    restart: unless-stopped
    expose: ["8093"]
    environment:
      UPDATER_TOKEN: ${UPDATER_TOKEN}
      DATABASE_URL: postgres://...
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ${PROJECT_DIR:-.}:${PROJECT_DIR:-.}
      - updater-state:/state
      - ./backups:/backups
```

Do not publish the port. Pinned versions live in `/state/versions.env`; a manual `docker compose up` without that file
falls back to `latest`, so pass `--env-file` for it if you mix manual and dashboard updates.

## Limitations

- The updater cannot update itself; pull a new `flixer-updater` image manually.
- The Docker socket is root-equivalent. Keep the port internal and the token secret.
- A component can only be rolled back if its previous version was a known release (not `dev` or `latest`).

## Development

```sh
go vet ./... && go test -race ./...
```
