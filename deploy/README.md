# Deploying syncd to the home server

`syncd` runs on the home server and is reached **only over Tailscale** — it is
never exposed to the public internet. Tailscale's WireGuard tunnel provides
transport encryption and network-level authentication, which is why syncd
itself speaks plain HTTP with bearer auth on top: TLS termination inside the
tailnet would add certificate management for no additional threat-model win.
(The archives are additionally AES-256-GCM-encrypted end-to-end by the app
before they ever leave the phone — syncd stores opaque blobs and never holds
a key.)

## Prerequisites

- A Linux home server with systemd, joined to the tailnet (`tailscale status`
  shows it), with the S24 Ultra's Tailscale client able to reach it.
- Go **not** required on the server if you cross-compile (step 1b).

## 1. Build

On the server (with Go ≥ 1.24):

```sh
git clone <this repo> && cd SumrakAPI
go build -o syncd ./cmd/syncd
```

Or cross-compile from the Mac and copy the binary over (pick the arch that
matches the server — `uname -m` there; `x86_64` → `amd64`, `aarch64` → `arm64`):

```sh
GOOS=linux GOARCH=amd64 go build -o syncd-linux ./cmd/syncd
scp syncd-linux <server>:/tmp/syncd
```

## 2. Install

```sh
sudo useradd --system --home /var/lib/syncd --shell /usr/sbin/nologin syncd
sudo install -m 755 /tmp/syncd /usr/local/bin/syncd

sudo mkdir -p /etc/syncd
sudo cp deploy/syncd.env.example /etc/syncd/syncd.env
sudo chmod 600 /etc/syncd/syncd.env
# Edit /etc/syncd/syncd.env: set SYNCD_TOKEN to `openssl rand -hex 32` output.

sudo cp deploy/syncd.service /etc/systemd/system/syncd.service
sudo systemctl daemon-reload
sudo systemctl enable --now syncd
```

`enable --now` both starts it and registers it for boot — that is the
reboot-survival requirement; verify with a real reboot below.

## 3. Verify

```sh
systemctl status syncd            # active (running)
journalctl -u syncd -f            # logs (stdout goes to the journal)

# From any tailnet machine (token from /etc/syncd/syncd.env):
curl -H "Authorization: Bearer $TOKEN" http://<tailnet-hostname>:8787/backups
# → {"backups":[]}
curl http://<tailnet-hostname>:8787/backups
# → 401 {"error":"unauthorized"}

# Reboot survival:
sudo reboot
# ...after it's back:
systemctl status syncd            # active (running) again, no manual step
```

The tailnet hostname is whatever `tailscale status` calls the server (e.g.
`homeserver` → `http://homeserver:8787`); MagicDNS names and the `100.x.y.z`
address both work. Use that same base URL + token in the app:
**Settings → Backup → Home server (syncd)**.

## Operational notes

- **Storage**: archives land in `/var/lib/syncd` as
  `sumrak-backup-YYYYMMDD-HHMMSSZ.json`, already encrypted by the app.
  Retention (30 newest daily dates + one per older month — the T20 rule)
  runs automatically after every upload; nothing else to cron.
- **Token rotation**: edit `/etc/syncd/syncd.env`, `sudo systemctl restart
  syncd`, update the token in the app.
- **Logs** never contain the token or archive contents.
- **Backup of the backups**: `/var/lib/syncd` is a copy of data that also
  lives on the phone and in the content repo's `backups/` — losing it only
  costs redundancy.

## Postgres + sync (`/v1`, T103)

Optional: turns on the per-row sync API for the two-phone account sync (design
`ACCOUNTS_AND_SYNC.md` §4/§6, option A: home server over Tailscale). The
backup routes are unaffected. On the home server:

```sh
# 1. Postgres
sudo apt install -y postgresql
sudo -u postgres psql -c "CREATE ROLE syncd_db LOGIN PASSWORD 'REPLACE_WITH_DB_PASSWORD';"
sudo -u postgres createdb -O syncd_db syncd

# 2. Config (root-only file): append the two vars
sudo tee -a /etc/syncd/syncd.env >/dev/null <<'ENVEOF'
SYNCD_DATABASE_URL=postgres://syncd_db:REPLACE_WITH_DB_PASSWORD@127.0.0.1:5432/syncd?sslmode=disable
SYNCD_FIREBASE_PROJECT_ID=sumrak-app
ENVEOF

# 3. Rebuild + install (same flow as steps 1-2 above), then restart.
#    Migrations run automatically at startup.
git pull && make build && sudo install -m 755 bin/syncd /usr/local/bin/syncd
#    (cross-compiling instead: make linux, scp bin/syncd-linux-* over, install it)
sudo systemctl restart syncd
journalctl -u syncd -n 20          # expect: migrations applied=[0001_init], syncEnabled=true

# 4. Verify
curl -H "Authorization: Bearer $TOKEN" http://<tailnet-hostname>:8787/backups   # still works
curl -i http://<tailnet-hostname>:8787/v1/me                                    # 401 without an ID token
```

Back up the database with `pg_dump syncd`: its rows hold the only server-side
copy of the other phone's changes. Other hosting options (managed Postgres +
Fly/Cloud Run, or Cloudflare Tunnel) differ only in `SYNCD_DATABASE_URL` and
reachability; the binary and env vars are identical. Never commit the real
password; `/v1` logs carry only a uid hash, counts and timings.

