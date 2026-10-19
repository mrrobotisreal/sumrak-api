# SumrakAPI — Repo Glossary

Repo-local terms for the Go backend (v1 scope = exactly `syncd`, design §9 /
ADR-0015). Cross-repo language lives in the workspace root `CONTEXT.md`
(Backup snapshot, syncd, Sync state, …).

## Language

**Archive / blob**:
What syncd stores — one backup snapshot file, already gzip+AES-256-GCM
encrypted by the app. Opaque to the server: syncd never decrypts, never
holds a key or passphrase (server compromise costs availability, not data).
_Avoid_: backup contents, payload (that's the app-side decrypted form)

**Backup name**:
The T20 file-name contract `sumrak-backup-YYYYMMDD-HHMMSSZ.json` (UTC).
The name is the archive's id in every endpoint, embeds the snapshot moment
(retention reasons from names alone, clock-independent), and doubles as the
path-traversal guard — anything not matching is rejected everywhere.

**Ring buffer (retention)**:
The T20 rule ported to Go: keep every archive from the 30 newest distinct
UTC dates present, plus the latest per older calendar month; never touch
unparseable names. Runs server-side after every successful `PUT /backup` —
the app never prunes this target.

**Bearer token**:
The single shared secret (`SYNCD_TOKEN` env, ≥16 chars) the app presents on
every request. Constant-time compared; every route — unknown paths included
— 401s without it. There is deliberately no unauthenticated endpoint, not
even a health check (an authed `GET /backups` is the health check).

**Base URL**:
How the app reaches the service: `https://sumrak.winapps.dev` — a Cloudflare
tunnel published route → `http://localhost:8687` on the home server, where it
runs as `sumrak-api.service` (NOT `syncd.service`: that name and :8787 belong
to the aflulit app on the same box). The service binds loopback only; the
bearer token (backups) and Firebase ID tokens (/v1) are the auth.

**Record**:
One synced row of one app SQLite table, stored in Postgres `records` as
`(uid, tbl, pk, data JSONB, deleted, hlc, device_id, seq)`. `tbl` must be on
the allowlist in `internal/sync/tables.go`. Unlike an archive, record data is
plaintext JSON to the server, scoped by Firebase `uid`.

**seq / cursor**:
`records.seq` comes from the global sequence `records_seq` and is bumped on
every applied write. `GET /v1/sync/pull?since=<seq>` returns rows with a
larger seq in order; `nextSince` is the client's cursor. Pushes are
serialised by an advisory lock so seq values commit in order and a cursor can
never skip a row.

**hlc**:
Hybrid logical clock, an int64 the app stamps on each change. Conflicts are
last-writer-wins on `(hlc, device_id)`, compared in SQL; the loser is
reported as `skipped`.

**Tombstone**:
A deleted record kept as a row (`deleted=true`, `data=NULL`) so the delete
syncs to other devices and a stale upsert (lower hlc) cannot resurrect it.

**Firebase ID token**:
The credential for `/v1/*` (not the backup bearer token). Verified offline
from Google's public certs and the project id; `uid` and `email` go into the
request context.

