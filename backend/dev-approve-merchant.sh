#!/bin/sh
# Dev only: approves a merchant application on the local dev database (g1dev), the way operations staff
# will once the operations console exists. Gives the shop its merchant number and payment QR.
#   ./dev-approve-merchant.sh <merchant id | TIN>
set -e
[ -n "$1" ] || { echo "usage: $0 <merchant id | TIN>"; exit 2; }
cd "$(dirname "$0")"
MSYS_NO_PATHCONV=1 docker run --rm --network g1net -v "$(pwd -W 2>/dev/null || pwd):/src" -v g1gomod:/go/pkg/mod \
  -v g1gocache:/root/.cache/go-build -w /src -e DATABASE_URL=postgres://postgres:test@g1db:5432/g1dev \
  -e GOTOOLCHAIN=auto golang:1.23.4 go run ./cmd/devtool approve-merchant "$1"
