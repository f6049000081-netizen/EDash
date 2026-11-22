#!/bin/sh
# Builds and starts the web business console in Docker on http://localhost:8070/business/, against the g1dev
# database (DATABASE_URL from backend/.env). Sign in by approving the code it shows from the app:
# Profile -> Business -> Sign in on a computer. The API server (run-dev.sh) must be running for the app.
set -e
cd "$(dirname "$0")"
[ -f .env ] || { echo "backend/.env is missing (see README)"; exit 1; }
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W 2>/dev/null || pwd):/src" -v g1gomod:/go/pkg/mod -v g1gocache:/root/.cache/go-build \
  -w /src -e CGO_ENABLED=0 golang:1.23.4 go build -o bin/business ./cmd/business
docker rm -f g1business >/dev/null 2>&1 || true
DSN=$(grep '^DATABASE_URL=' .env | cut -d= -f2-)
MSYS_NO_PATHCONV=1 docker run -d --name g1business --network g1net -p 8070:8070 -e DATABASE_URL="$DSN" \
  -e BUSINESS_BASE_URL=http://localhost:8070 -v "$(pwd -W 2>/dev/null || pwd)/bin:/app:ro" golang:1.23.4 /app/business >/dev/null
sleep 2 && curl -s http://localhost:8070/healthz && echo " - business console on http://localhost:8070/business/"
