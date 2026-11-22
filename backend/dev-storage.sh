#!/bin/sh
# Dev only: starts an S3-compatible store (SeaweedFS, container g1s3) for KYB documents, and writes
# backend/.s3/docs.env with its settings and a document encryption key. Both are made once, with random
# secrets, and are git-ignored. run-dev.sh and run-ops-dev.sh call this; it's safe to run again.
set -e
cd "$(dirname "$0")"
SRC="$(pwd -W 2>/dev/null || pwd)"
rand() { python -c "import secrets; print(secrets.token_urlsafe($1))"; }

if [ ! -f .s3/docs.env ]; then
  mkdir -p .s3
  SECRET=$(rand 24)
  printf '{"identities":[{"name":"g1dev","credentials":[{"accessKey":"g1dev","secretKey":"%s"}],"actions":["Admin","Read","Write","List"]}]}\n' "$SECRET" > .s3/s3.json
  cat > .s3/docs.env <<EOF
S3_ENDPOINT=g1s3:8333
S3_BUCKET=g1-kyb-dev
S3_ACCESS_KEY=g1dev
S3_SECRET_KEY=$SECRET
S3_INSECURE=true
DOCS_KEYS=dev1:$(python -c "import secrets, base64; print(base64.b64encode(secrets.token_bytes(32)).decode())")
EOF
  docker rm -f g1s3 >/dev/null 2>&1 || true
fi

if [ -z "$(docker ps -q -f name=^g1s3$)" ]; then
  docker rm -f g1s3 >/dev/null 2>&1 || true
  # Files persist in the g1s3data volume, so documents survive a restart (the database is the throwaway part).
  MSYS_NO_PATHCONV=1 docker run -d --name g1s3 --network g1net -p 127.0.0.1:8333:8333 -v g1s3data:/data -v "$SRC/.s3:/etc/g1s3:ro" \
    chrislusf/seaweedfs:3.80 server -dir=/data -s3 -s3.port=8333 -s3.config=/etc/g1s3/s3.json >/dev/null
fi
# Ready when the S3 port answers (403: it wants signed requests).
for i in $(seq 1 30); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8333/)" = 403 ] && exit 0
  sleep 1
done
echo "document storage (g1s3) didn't start"; docker logs g1s3 2>&1 | tail -5; exit 1
