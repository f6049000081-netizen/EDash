#!/bin/sh
# Builds and starts the dev API in Docker on port 8080, against the g1dev database in g1db.
# Settings and keys come from backend/.env (git-ignored; APP_ENV=dev, DATABASE_URL, AUTH_SIGNING_KEY, OTP_HMAC_KEY).
# Sign-in codes are not sent in dev: read them with
#   docker logs -f g1api 2>&1 | grep "dev sms"
set -e
cd "$(dirname "$0")"
[ -f .env ] || { echo "backend/.env is missing (see README)"; exit 1; }
./dev-storage.sh
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W 2>/dev/null || pwd):/src" -v g1gomod:/go/pkg/mod -v g1gocache:/root/.cache/go-build \
  -w /src -e CGO_ENABLED=0 golang:1.23.4 go build -o bin/api ./cmd/api
docker rm -f g1api >/dev/null 2>&1 || true
MSYS_NO_PATHCONV=1 docker run -d --name g1api --network g1net -p 8080:8080 --env-file .env --env-file .s3/docs.env \
  -v "$(pwd -W 2>/dev/null || pwd)/bin:/app:ro" golang:1.23.4 /app/api >/dev/null
sleep 2 && curl -s http://localhost:8080/healthz && echo " - API up on :8080"
