#!/usr/bin/env bash
# Brings the vendor stack up, including the identity provider's bootstrap.
#
# THE ORDER IS THE POINT, and it is a genuine cycle rather than fussiness. The
# console and the control plane cannot start without Logto application ids and
# secrets; those do not exist until the seed runs; the seed cannot run until
# Logto is up and reachable AT ITS PUBLIC NAME, because the ids it mints are
# bound to the URLs it is told about. So: identity first, seed, then everything
# that depends on it.
#
# Idempotent. The seed converges what it can, and re-running this against a live
# stack re-reads the same ids rather than minting new ones.
set -euo pipefail
cd "$(dirname "$0")"

[ -f .env ] || { echo "no .env -- copy .env.example and fill it in" >&2; exit 1; }
set -a; . ./.env; set +a
: "${BASE:?set BASE in .env}"

compose() { docker compose "$@"; }

# The guard on admin. and mail., generated once and kept.
#
# Its own file rather than more entries in .env, because a bcrypt hash is full
# of `$` and compose interpolates `environment:` values. env_file is passed
# through literally.
if [ ! -f caddy-auth.creds ]; then
  echo "==> generating credentials for the guarded surfaces"
  ADMIN_PW=$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | head -c 24)
  HASH=$(docker run --rm caddy:2-alpine caddy hash-password --plaintext "$ADMIN_PW")
  umask 077
  # Caddyfile tokens, because Caddy imports this straight into the basic_auth
  # block. Never through compose: see the Caddyfile's header.
  printf 'admin %s\n' "$HASH" > caddy-auth.creds
  # The plaintext is written ONCE, next to the hash, because there is nowhere
  # else to recover it from -- bcrypt is one-way and regenerating it would lock
  # out whoever is already using it.
  printf 'admin\n%s\n' "$ADMIN_PW" > .caddy-auth.plaintext
  echo "    admin / $ADMIN_PW   (also in deploy/.caddy-auth.plaintext)"
fi

echo "==> identity and proxy"
compose up -d db logto mailpit caddy
# The Caddyfile is a BIND MOUNT, so editing it changes nothing compose can see:
# the container spec is identical and `up -d` leaves it running with the old
# config in memory. Reload explicitly, or a routing change appears to have been
# ignored.
compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile 2>/dev/null \
  || compose restart caddy

echo "==> waiting for logto at https://auth.$BASE"
for i in $(seq 1 60); do
  if curl -fsk -o /dev/null "https://auth.$BASE/oidc/.well-known/openid-configuration"; then
    echo "    up"; break
  fi
  [ "$i" = 60 ] && { echo "logto never became reachable" >&2; exit 1; }
  sleep 5
done

# Read the bootstrap credential ourselves and hand it to the seed.
#
# The seed can do this on its own, but only by name: it shells out to
# `docker exec auth-stack-db-1`, which is the LOCAL stack's container. Passing
# LOGTO_M2M_SECRET is the documented escape hatch and means the seed needs no
# docker socket, so it can run in a throwaway container on the compose network.
echo "==> reading the logto bootstrap secret"
M2M=$(compose exec -T db psql -U postgres -d logto -t -A \
  -c "select secret from applications where id='m-default';" | tr -d '\r')
[ -n "$M2M" ] || { echo "could not read the m-default secret" >&2; exit 1; }

# The admin endpoint is the PUBLIC one even though it is guarded, because Logto
# resolves its tenant from the request host: asked at http://logto:3002 it does
# not recognise m-default and answers "invalid client", which reads like a wrong
# secret. The Caddyfile exempts /oidc/token from the guard for exactly this, and
# that endpoint is not an open door -- it requires the m2m client secret, which
# lives in the database on the host.

# INVERTED, deliberately spelled out. SEED_TLS_INSECURE=1 means "accept Caddy's
# own CA", while NODE_TLS_REJECT_UNAUTHORIZED=1 means "verify strictly" -- so
# passing one straight into the other reads correctly and does the opposite.
REJECT=1
[ "${SEED_TLS_INSECURE:-0}" = "1" ] && REJECT=0

echo "==> seeding logto for https://console.$BASE"
compose run --rm --no-deps -T \
  -v "$(cd .. && pwd)/auth-stack:/seed" -w /seed \
  -e LOGTO_M2M_SECRET="$M2M" \
  -e LOGTO_ENDPOINT="https://auth.$BASE" \
  -e LOGTO_ADMIN_ENDPOINT="https://admin.$BASE" \
  -e CONSOLE_REDIRECT="https://console.$BASE/api/auth/callback" \
  -e CONSOLE_POST_LOGOUT="https://console.$BASE/" \
  -e LEMUL_API="http://controlplane:9000" \
  -e SMTP_HOST="${SMTP_HOST:-mailpit}" \
  -e SMTP_PORT="${SMTP_PORT:-1025}" \
  -e SMTP_USER="${SMTP_USER:-lemul}" \
  -e SMTP_PASS="${SMTP_PASS:-lemul}" \
  -e SMTP_SECURE="${SMTP_SECURE:-false}" \
  -e SMTP_FROM="${SMTP_FROM:-lemul@localhost}" \
  -e SIGNUP_ALLOWLIST="${SIGNUP_ALLOWLIST:-}" \
  -e NODE_TLS_REJECT_UNAUTHORIZED="$REJECT" \
  seeder bun seed.ts > .env.generated

echo "==> merging generated identity into .env"
# The seed's output is authoritative for everything it emits, so generated keys
# REPLACE rather than append. Appending would leave two values for one key and
# let the stale one win depending on who reads the file.
python3 - <<'PY'
import re
gen = {}
for line in open('.env.generated'):
    line = line.strip()
    if line and not line.startswith('#') and '=' in line:
        k, v = line.split('=', 1)
        gen[k] = v
out, seen = [], set()
for line in open('.env'):
    m = re.match(r'([A-Z0-9_]+)=', line)
    if m and m.group(1) in gen:
        k = m.group(1)
        out.append(f'{k}={gen[k]}\n')
        seen.add(k)
    else:
        out.append(line)
if out and not out[-1].endswith('\n'):
    out.append('\n')
for k, v in gen.items():
    if k not in seen:
        out.append(f'{k}={v}\n')
open('.env', 'w').writelines(out)
PY

# Re-read BEFORE checking. The values were written to .env by the merge above,
# not to this shell's environment, so checking first tests the file as it was
# when the script started.
set -a; . ./.env; set +a
for k in LOGTO_APP_ID LOGTO_APP_SECRET LOGTO_COOKIE_SECRET \
         LEMUL_CLI_CLIENT_ID LEMUL_CONSOLE_CLIENT_ID; do
  [ -n "${!k:-}" ] || { echo "seed produced no $k" >&2; exit 1; }
done

echo "==> the rest of the stack"
compose up -d

AUTH_NOTE=""
[ -f .caddy-auth.plaintext ] && AUTH_NOTE="  guarded  admin / $(sed -n 2p .caddy-auth.plaintext)"

cat <<EOF

  console   https://console.$BASE
  mail      https://mail.$BASE        (sign-in codes land here -- GUARDED)
  admin     https://admin.$BASE       (logto admin -- GUARDED)
$AUTH_NOTE
  api       https://cp.$BASE
  relay     wss://relay.$BASE

Point a runner at it with:
  control_plane_url = "wss://cp.$BASE"
EOF
