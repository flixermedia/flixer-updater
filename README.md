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

All `/v1` routes need `Authorization: Bearer <token>`. The token is derived automatically from the stream token secret shared on the `flixer-secrets` volume (`hex(HMAC-SHA256(secret, "flixer-updater-token"))`), so nothing has to be configured.

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
| `STREAM_TOKEN_SECRET_FILE` | required unless `UPDATER_TOKEN` is set | The shared secret file; read per request so it may appear after start. |
| `UPDATER_TOKEN`        | unset        | Optional explicit override (16+ characters); must then also be set on the Flixer API. |
| `PORT`                 | `8093`       |                                                                    |
| `DATABASE_URL`         | unset        | Optional fallback. Normally Flixer sends its own database URL with each update request. |
| `BACKUP_DIR`           | `/backups`   | Holds legacy `.dump` files, Flixer-written `.flxbak` archives, and restore safety backups. |
| `BACKUP_PASSPHRASE_FILE` | `<BACKUP_DIR>/.passphrase` | Written by Flixer. Contents are the passphrase (one trailing newline stripped); missing or empty means plain archives. Read fresh per operation. |
| `CONFIG_DIR`           | `/var/lib/flixer/config` | SQLite config DB, WAL/SHM files, and secret-box key. |
| `SECRETS_DIR`          | `/run/flixer-secrets` | Shared Flixer secret files; mounted read-write for restore. |
| `METADATA_DIR`         | `/data/metadata` | Indexer cache, subtitles, and uploaded artwork. |
| `STATE_DIR`            | `/state`     | `state.json` and `versions.env`. |
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
      STREAM_TOKEN_SECRET_FILE: /run/flixer-secrets/stream-token-secret
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - flixer-secrets:/run/flixer-secrets
      - flixer-config:/var/lib/flixer/config
      - ${DATA_PATH:?Set DATA_PATH}/metadata:/data/metadata
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

## Restore (Flixer-written `.flxbak` archives)

Flixer creates, lists, schedules, uploads, downloads, deletes and encrypts backups itself. The updater only restores them, because restore needs Docker control of the services. Route: `POST /v1/backups/{name}/restore` with body `{"databaseUrl":"...","passphrase":"optional","components":["database","config","secrets","metadata"]}`. `{name}` must match `^flixer-[0-9TZ-]+\.flxbak$` and exist in `BACKUP_DIR`; otherwise `404`. `components` is optional and only restores components that are also in the archive.

Passphrase: the request `passphrase` if non-empty, otherwise the contents of `BACKUP_PASSPHRASE_FILE`. Errors are JSON `{"error":"...","code":"..."}`: `400 bad_passphrase`, `400 corrupt`, `409 busy`. The archive is validated and decrypted before any service is touched.

Flow (steps in the run): `safety-backup` of the current state (`flixer-<ts>.flxbak`, encrypted only if the passphrase file has content; written under a non-matching temp name and renamed; nothing else in `BACKUP_DIR` is touched or pruned; if it fails the restore aborts) -> `stop-services` (`api`, `indexer`, `streamer`; postgres/redis keep running) -> `restore-database` (`pg_restore --clean --if-exists --no-owner --single-transaction`) -> `restore-config` / `restore-secrets` / `restore-metadata` (target directory contents are fully replaced, so stale `-wal`/`-shm` files are removed; modes limited to 0600) -> `start-services` and health waits. After a failure past the stop, services are still started again and the error names the safety backup.

The run JSON (`/v1/status`, and the `202` response) has `kind` (`update` | `restore`), `encrypted` (the archive's flag), `backup` (safety backup name), `step` and `steps: [{name,message,status,startedAt,finishedAt}]`. Update runs keep their previous shape plus `kind:"update"`. One operation runs at a time, shared with updates.

### Archive format

`FLXBAK1\n` (magic), uint32 big-endian header length, JSON header (unencrypted), then the body. Header: `{"createdAt":RFC3339,"components":[...],"encrypted":bool,"versions":{...},"kdf":{"name":"pbkdf2-sha256","iterations":600000,"salt":b64(16 bytes),"noncePrefix":b64(4 bytes),"check":b64(optional)}}`. `kdf` is only needed when encrypted. The body is a tar.gz with entries `manifest.json` (`{"version":1,"createdAt","components","versions",...}`; only `version` 1 and the component set are checked), `database.dump` (`pg_dump --format=custom --no-owner`), `secrets/<file>`, `config/<file>`, `metadata/<relative path>`. Directory entries (with or without a trailing slash) and PAX global headers are accepted; absolute paths, `..`, symlinks and hardlinks are rejected.

Encrypted body: key = PBKDF2-HMAC-SHA256(passphrase, salt, 600000, 32 bytes); the plaintext tar.gz is split into 64 KiB chunks, each written as `[flag byte: 1 for the last chunk else 0][uint32 BE ciphertext length][AES-256-GCM ciphertext+tag]`, nonce = `noncePrefix` (4 bytes) + 8-byte big-endian chunk counter from 0, AAD = the exact header JSON bytes. A missing final-flag chunk means truncation (`corrupt`). `check` (optional) = b64(HMAC-SHA256(key, "flixer-backup-passphrase-check")) gives a fast wrong-passphrase answer; without it, failure of the first chunk is reported as `bad_passphrase`.

The config component should be a self-contained SQLite snapshot plus key files; restore deletes everything else in `CONFIG_DIR`.

Env: `BACKUP_DIR`, `BACKUP_PASSPHRASE_FILE`, `CONFIG_DIR` (`/var/lib/flixer/config`), `SECRETS_DIR` (`/run/flixer-secrets`), `METADATA_DIR` (`/data/metadata`).

**Security:** the Docker socket and the read/write secrets/config/metadata mounts are root-equivalent. Keep the API internal and the bearer token protected. The passphrase is read from the file per operation, held only in memory, and never logged, archived or returned in run JSON. Restoring `secrets` changes the derived updater token; the token is re-read per request, so this takes effect immediately.
