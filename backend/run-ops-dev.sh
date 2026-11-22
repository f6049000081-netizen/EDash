#!/bin/sh
# Dev only: starts the operations console on http://localhost:8090/ops/ against the g1dev database, with
# Dex (container g1dex) standing in for the company identity provider.
#
# The first run creates backend/.dex/ (git-ignored): the Dex config with two dev staff accounts and random
# passwords (written to backend/.dex/passwords.txt), and invites both as staff in g1dev:
#   tsion.admin@gebeya.dev   super admin
#   dawit.risk@gebeya.dev    risk analyst and finance officer
#   selam.admin@gebeya.dev   super admin (a second one, so staff changes can be approved)
# Several people, so requests that need a second person can be approved. The dev identity provider has no
# hardware MFA, so OPS_REQUIRED_AMR is empty here (APP_ENV=dev); production refuses to start without it.
set -e
cd "$(dirname "$0")"
SRC="$(pwd -W 2>/dev/null || pwd)"
GO="docker run --rm -v $SRC:/src -v g1gomod:/go/pkg/mod -v g1gocache:/root/.cache/go-build -w /src"
# The console runs in Dex's network namespace, so http://localhost:5556/dex is the same issuer for the
# browser on this machine and for the console's server-side calls.
ISSUER=http://localhost:5556/dex
rand() { python -c "import secrets; print(secrets.token_urlsafe($1))"; }

if [ ! -f .dex/config.yaml ] || ! grep -q selam.admin .dex/config.yaml; then
  mkdir -p .dex
  CLIENT_SECRET=$(rand 32)
  P1=$(rand 12); P2=$(rand 12); P3=$(rand 12)
  H1=$(printf %s "$P1" | MSYS_NO_PATHCONV=1 $GO -i golang:1.23.4 go run ./cmd/devtool hash-password)
  H2=$(printf %s "$P2" | MSYS_NO_PATHCONV=1 $GO -i golang:1.23.4 go run ./cmd/devtool hash-password)
  H3=$(printf %s "$P3" | MSYS_NO_PATHCONV=1 $GO -i golang:1.23.4 go run ./cmd/devtool hash-password)
  cat > .dex/config.yaml <<EOF
issuer: $ISSUER
storage: {type: memory}
web: {http: 0.0.0.0:5556}
oauth2: {skipApprovalScreen: true}
staticClients:
  - id: g1ops
    name: EDash operations (dev)
    secret: $CLIENT_SECRET
    redirectURIs: ['http://localhost:8090/ops/callback']
enablePasswordDB: true
staticPasswords:
  - {email: tsion.admin@gebeya.dev, username: Tsion, userID: 7d1f1b52-6a43-4f3e-9d0e-6a1f2d5b0c01, hash: '$H1'}
  - {email: dawit.risk@gebeya.dev, username: Dawit, userID: 7d1f1b52-6a43-4f3e-9d0e-6a1f2d5b0c02, hash: '$H2'}
  - {email: selam.admin@gebeya.dev, username: Selam, userID: 7d1f1b52-6a43-4f3e-9d0e-6a1f2d5b0c03, hash: '$H3'}
EOF
  printf 'tsion.admin@gebeya.dev  %s\ndawit.risk@gebeya.dev   %s\n' "$P1" "$P2" > .dex/passwords.txt
  printf 'selam.admin@gebeya.dev  %s\n' "$P3" >> .dex/passwords.txt
  cat > .dex/ops.env <<EOF
APP_ENV=dev
DATABASE_URL=postgres://postgres:test@g1db:5432/g1dev
OPS_BASE_URL=http://localhost:8090
OIDC_ISSUER=$ISSUER
OIDC_CLIENT_ID=g1ops
OIDC_CLIENT_SECRET=$CLIENT_SECRET
OPS_SESSION_KEY=$(python -c "import secrets, base64; print(base64.b64encode(secrets.token_bytes(32)).decode())")
EOF
  for s in "tsion.admin@gebeya.dev|Tsion Haile|super_admin" "dawit.risk@gebeya.dev|Dawit Bekele|risk_analyst,finance_officer" \
           "selam.admin@gebeya.dev|Selam Girma|super_admin"; do
    IFS='|' read -r EMAIL NAME ROLES <<EOF2
$s
EOF2
    MSYS_NO_PATHCONV=1 $GO --network g1net -e DATABASE_URL=postgres://postgres:test@g1db:5432/g1dev golang:1.23.4 \
      go run ./cmd/devtool invite-staff "$EMAIL" "$NAME" "$ROLES" </dev/null
  done
fi

./dev-storage.sh
docker rm -f g1dex >/dev/null 2>&1 || true
MSYS_NO_PATHCONV=1 docker run -d --name g1dex --network g1net -p 127.0.0.1:5556:5556 -p 127.0.0.1:8090:8090 -v "$SRC/.dex:/etc/dex:ro" \
  ghcr.io/dexidp/dex:v2.41.1 dex serve /etc/dex/config.yaml >/dev/null

MSYS_NO_PATHCONV=1 docker run --rm -v "$SRC:/src" -v g1gomod:/go/pkg/mod -v g1gocache:/root/.cache/go-build \
  -w /src -e CGO_ENABLED=0 golang:1.23.4 go build -o bin/ops ./cmd/ops
docker rm -f g1ops >/dev/null 2>&1 || true
MSYS_NO_PATHCONV=1 docker run -d --name g1ops --network container:g1dex --env-file .dex/ops.env --env-file .s3/docs.env \
  -v "$SRC/bin:/app:ro" golang:1.23.4 /app/ops >/dev/null
for i in 1 2 3 4 5 6 7 8 9 10; do curl -sf http://localhost:8090/healthz >/dev/null && break; sleep 2; done
curl -sf http://localhost:8090/healthz >/dev/null && echo "ok - console on http://localhost:8090/ops/ (passwords in backend/.dex/passwords.txt)" \
  || { docker logs g1ops 2>&1 | tail -5; exit 1; }
