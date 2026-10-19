# Deploying the Sumrak API to the home server

The binary is built from `cmd/syncd` (the T21 name) but installs and runs as
**`sumrak-api`**. The home server already runs the **aflulit** (Hebrew) app's
own `syncd.service` on **:8787**, so everything Sumrak is namespaced and the
two never touch:

| | aflulit (existing, untouched) | **Sumrak** |
|---|---|---|
| systemd unit | `syncd.service` | **`sumrak-api.service`** |
| binary | `/usr/local/bin/syncd` | **`/usr/local/bin/sumrak-api`** |
| system user | `syncd` | **`sumrak-api`** |
| env file | `/etc/syncd/syncd.env` | **`/etc/sumrak-api/sumrak-api.env`** |
| data dir | `/var/lib/syncd` | **`/var/lib/sumrak-api`** |
| listen | `:8787` | **`127.0.0.1:8687`** |
| Postgres | — | role + db **`sumrak_api`** |
| public URL | — | **`https://sumrak.winapps.dev`** (Cloudflare tunnel → `http://localhost:8687`) |

Reachability: the Cloudflare tunnel's published application route
`sumrak.winapps.dev` → `http://localhost:8687` gives TLS and a public
hostname; the service itself binds loopback only. Auth is unchanged:
backups need the `SYNCD_TOKEN` bearer token, `/v1/*` needs a Firebase ID
token for project `sumrak-app` — there is no unauthenticated route.
(Backup archives are additionally AES-256-GCM-encrypted by the app.)

## First-time install

```sh
# 0. Code + build (Go ≥ 1.24 on the server; or `make linux` on the Mac and scp)
cd ~ && git clone git@github.com:mrrobotisreal/sumrack-api.git SumrakAPI   # or: cd SumrakAPI && git pull
cd SumrakAPI && make build                     # → bin/sumrak-api

# 1. Postgres (skip the install line if it's already there)
sudo apt install -y postgresql
sudo -u postgres psql -c "CREATE ROLE sumrak_api LOGIN PASSWORD 'REPLACE_WITH_DB_PASSWORD';"
sudo -u postgres createdb -O sumrak_api sumrak_api

# 2. User, binary, config
sudo useradd --system --home /var/lib/sumrak-api --shell /usr/sbin/nologin sumrak-api
sudo install -m 755 bin/sumrak-api /usr/local/bin/sumrak-api
sudo mkdir -p /etc/sumrak-api
sudo cp deploy/sumrak-api.env.example /etc/sumrak-api/sumrak-api.env
sudo chmod 600 /etc/sumrak-api/sumrak-api.env
sudoedit /etc/sumrak-api/sumrak-api.env
#   SYNCD_TOKEN        ← `openssl rand -hex 32`
#   SYNCD_DATABASE_URL ← the password from step 1
#   (SYNCD_ADDR=127.0.0.1:8687 and SYNCD_FIREBASE_PROJECT_ID=sumrak-app are pre-set)

# 3. Service
sudo cp deploy/sumrak-api.service /etc/systemd/system/sumrak-api.service
sudo systemctl daemon-reload
sudo systemctl enable --now sumrak-api
journalctl -u sumrak-api -n 20     # expect: migrations applied=[0001_init], syncEnabled=true

# 4. Cloudflare tunnel: published application route
#    sumrak.winapps.dev  →  http://localhost:8687
#    (Zero Trust dashboard → Networks → Tunnels → <tunnel> → Public Hostname,
#     or the ingress rule in the tunnel's config.yml, then restart cloudflared)
```

## Verify

```sh
systemctl status sumrak-api syncd     # BOTH active — aflulit's syncd untouched on :8787
ss -ltnp | grep -E ':8687|:8787'      # sumrak-api on 127.0.0.1:8687, aflulit on :8787

TOKEN=$(sudo grep ^SYNCD_TOKEN= /etc/sumrak-api/sumrak-api.env | cut -d= -f2)
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8687/backups   # {"backups":[]}
curl -s -H "Authorization: Bearer $TOKEN" https://sumrak.winapps.dev/backups
curl -si https://sumrak.winapps.dev/v1/me | head -1                      # HTTP/2 401 (no ID token)

sudo reboot   # …then: systemctl status sumrak-api → active again, no manual step
```

In the app: **Settings → Backup → Home server** = `https://sumrak.winapps.dev` +
the token; **Settings → Аккаунт** sync uses the same base URL with the
Firebase sign-in (no token needed).

## Updating later

```sh
cd ~/SumrakAPI && git pull && make build \
  && sudo install -m 755 bin/sumrak-api /usr/local/bin/sumrak-api \
  && sudo systemctl restart sumrak-api && journalctl -u sumrak-api -n 10
```

Migrations run automatically at startup (advisory-locked, idempotent).

## Operational notes

- **Storage**: backup archives land in `/var/lib/sumrak-api` as
  `sumrak-backup-YYYYMMDD-HHMMSSZ.json` (already encrypted by the app);
  retention (30 newest daily dates + one per older month, the T20 rule) runs
  after every upload.
- **Database**: `sudo -u postgres pg_dump sumrak_api > sumrak_api.sql` — its
  rows are the server-side copy of each phone's changes.
- **Token rotation**: edit `/etc/sumrak-api/sumrak-api.env`,
  `sudo systemctl restart sumrak-api`, update the token in the app.
- **Logs** never contain the token, ID tokens, archive contents or row data —
  `/v1` lines carry a uid hash, counts and timings only.
