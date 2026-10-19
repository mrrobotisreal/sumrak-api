# Сумрак (Sumrak) API — `syncd`

The Go backend for [Sumrak](../Sumrak) (Mitch's personal Russian-learning
app). v1 scope is exactly **`syncd`**, the backup-sync service from design §9
(ADR-0015): it runs on the home server as `sumrak-api.service` on
`127.0.0.1:8687`, is reached at `https://sumrak.winapps.dev` (Cloudflare
tunnel), and
stores the app's **encrypted** backup snapshots as opaque blobs. It never
decrypts anything — the passphrase and derived key exist only on the phone.

## Endpoints (design §9 contract)

| Route | Purpose |
|---|---|
| `PUT /backup?name=<file>` | Store one archive (create-only; 409 on duplicate). Runs retention afterwards. |
| `GET /backups` | List stored archives `{name, size, timestampMs}`, newest first. |
| `GET /backup/{name}` | Return one archive verbatim. |

Every route (including unknown paths) requires `Authorization: Bearer
<token>`; there is deliberately no unauthenticated endpoint, not even a
health check — an authed `GET /backups` is the
health check.

Archive names are the T20 contract shared with the app and the GitHub
target: `sumrak-backup-YYYYMMDD-HHMMSSZ.json` (UTC). The name is the id.
Retention is the T20 ring buffer: keep every backup from the 30 newest
distinct UTC dates, plus the latest per older calendar month.

## `/v1` per-row sync API (T103, M29)

Optional: enabled only when `SYNCD_FIREBASE_PROJECT_ID` is set (then
`SYNCD_DATABASE_URL` is required). With it unset, `/v1/*` 404s and syncd is
backup-only exactly as before. `/v1/*` is authenticated with a **Firebase ID
token** (`Authorization: Bearer <ID token>`), not the backup bearer token; the
backup routes keep the bearer token. All bodies are JSON; request bodies may be
`Content-Encoding: gzip`; responses are gzipped when `Accept-Encoding: gzip`.

| Route | Request | Response |
|---|---|---|
| `POST /v1/devices` | `{deviceId, name, model, appVersion}` | `{uid, email, deviceCount, recordCount, maxSeq}` |
| `GET /v1/me` | none | same + `devices:[{deviceId, name, model, appVersion, lastSeenAt}]` |
| `POST /v1/sync/push` | `{deviceId, schemaVersion, changes:[{tbl, pk, op:"upsert"\|"delete", data?, hlc}]}` (max 500 changes, max `SYNCD_MAX_PUSH_BYTES`, 413 beyond) | `{applied, skipped, maxSeq}` |
| `GET /v1/sync/pull?since=<seq>&limit=<=1000` (default 500) | none | `{changes:[{tbl, pk, op, data?, hlc, deviceId, seq}], nextSince, hasMore}` |

Last-writer-wins is decided in SQL by `(hlc, device_id)`; a loser is counted in
`skipped`. Deletes are tombstones. Pull returns the caller's own rows too
(clients drop echoes by `deviceId`). Unknown `tbl` gives 400 (allowlist in
`internal/sync/tables.go`, pinned by a test; the app registry must match).
Errors are `{"error": "..."}`.

**Config:** `SYNCD_DATABASE_URL`, `SYNCD_FIREBASE_PROJECT_ID`,
`SYNCD_MAX_PUSH_BYTES` (default 5242880). See `deploy/sumrak-api.env.example`.

**Library choice:** `github.com/golang-jwt/jwt/v5` + stdlib `crypto/x509`
verifying against Google's public securetoken x509 certs (cached by
`Cache-Control: max-age`), not `firebase.google.com/go/v4`: the Admin SDK
would drag in a large dependency tree and invite service-account credentials,
whereas verification needs only the project id. Checks: RS256 only, `iss`,
`aud`, `exp`, `iat` / `auth_time` not in the future (5 min skew), `sub` 1-128
chars. Postgres driver: `github.com/jackc/pgx/v5` (pgxpool).

**Tests:** `make test` runs everything that needs no database;
`make test-pg` creates a throwaway DB on local Postgres 17, sets
`SYNCD_TEST_DATABASE_URL`, runs the integration tests, and drops the DB.

**Curl walkthrough** (dev build only: the `SYNCD_DEV_CERTS_URL` override and
`cmd/devtoken` exist solely under `-tags dev`):

```sh
createdb syncd_dev
go build -tags dev -o /tmp/syncd-dev ./cmd/syncd && go build -tags dev -o /tmp/devtoken ./cmd/devtoken
/tmp/devtoken -project sumrak-dev > /tmp/tok.txt &     # prints a token, then serves fake certs on :9099
SYNCD_TOKEN=$(openssl rand -hex 32) SYNCD_ADDR=127.0.0.1:8788 SYNCD_DATA_DIR=/tmp/syncd-data \
  SYNCD_DATABASE_URL="postgres://$USER@localhost:5432/syncd_dev?sslmode=disable" \
  SYNCD_FIREBASE_PROJECT_ID=sumrak-dev SYNCD_DEV_CERTS_URL=http://127.0.0.1:9099/certs /tmp/syncd-dev &
T=$(cat /tmp/tok.txt); B=http://127.0.0.1:8788
curl -i $B/v1/me                                                   # 401
curl -H "Authorization: Bearer $T" -X POST $B/v1/devices -d '{"deviceId":"dev-A","name":"S24U","model":"SM-S928U","appVersion":"1.19.0"}'
curl -H "Authorization: Bearer $T" -X POST $B/v1/sync/push -d '{"deviceId":"dev-A","schemaVersion":1,"changes":[{"tbl":"notes","pk":"n1","op":"upsert","hlc":1,"data":{"body":"hi"}}]}'
curl -H "Authorization: Bearer $T" "$B/v1/sync/pull?since=0"
```

## Layout

```
cmd/syncd/          entrypoint
internal/config/    env-var configuration (SYNCD_TOKEN, SYNCD_ADDR, SYNCD_DATA_DIR, …)
internal/auth/      bearer-token middleware (constant-time)
internal/db/        pgxpool + embedded migrations (advisory-locked)
internal/firebaseauth/ Firebase ID-token verifier + middleware
internal/sync/      /v1 allowlist, push/pull, devices/me
internal/naming/    backup file-name parsing (shared contract with the app)
internal/retention/ T20 ring-buffer retention, ported
internal/storage/   disk blob store (atomic create-only writes)
internal/server/    routing + handlers (stdlib net/http)
deploy/             systemd unit, env template, deploy walkthrough
```

Router: stdlib `net/http` (Go 1.22+ method/path patterns). The only
dependencies are pgx and golang-jwt, needed by the optional `/v1` sync API.

## Develop

```sh
make check       # gofmt + go vet + go test
make build       # → bin/sumrak-api

SYNCD_TOKEN=$(openssl rand -hex 32) SYNCD_DATA_DIR=./data go run ./cmd/syncd
```

Configuration is env-vars only (see `deploy/sumrak-api.env.example`); no secret is
ever committed or logged.

## Deploy

See [`deploy/README.md`](deploy/README.md) — build, systemd install,
the side-by-side table with aflulit's `syncd` (:8787), the Cloudflare tunnel
route, reboot-survival verification.
