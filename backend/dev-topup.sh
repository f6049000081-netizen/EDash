#!/bin/sh
# Dev only: adds sandbox test money to an account on the local dev API (run-dev.sh), creating the account
# if the number hasn't signed in yet. The credit appears in activity as "Test money (sandbox)".
#   ./dev-topup.sh +251911234567 5000 ["Display name"]
# It signs in as that number by reading the dev SMS code from the g1api log, tops up, then signs out.
set -e
PHONE=$1; ETB=$2; NAME=$3; B=${API:-http://localhost:8080}
[ -n "$PHONE" ] && [ -n "$ETB" ] || { echo "usage: $0 +2519XXXXXXXX ETB [name]"; exit 2; }
j() { python -c "import sys,json; print(json.load(sys.stdin)$1)"; }
uuid() { python -c "import uuid; print(uuid.uuid4())"; }
CH=$(curl -sf -X POST $B/v1/auth/otp -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE\",\"purpose\":\"login\",\"locale\":\"en\"}" | j "['challenge_id']")
sleep 1
LAST4=$(printf %s "$PHONE" | tail -c 4)
CODE=$(docker logs g1api 2>&1 | grep '"dev sms' | grep "\*\*\*\*$LAST4" | tail -1 | grep -oE '[0-9]{6}' | tail -1)
TOKEN=$(curl -sf -X POST $B/v1/auth/otp/verify -H 'Content-Type: application/json' \
  -d "{\"challenge_id\":\"$CH\",\"code\":\"$CODE\",\"device\":{\"platform\":\"android\",\"model\":\"dev-topup script\",\"attestation\":\"dev-topup-script\"}}" | j "['access_token']")
AUTH="Authorization: Bearer $TOKEN"
if [ -n "$NAME" ]; then
  curl -sf -X PATCH $B/v1/me -H "$AUTH" -H 'Content-Type: application/json' -d "{\"display_name\":\"$NAME\"}" >/dev/null
fi
MINOR=$(python -c "from decimal import Decimal; print(int(Decimal('$ETB')*100))")
curl -sf -X POST $B/v1/sandbox/topups -H "$AUTH" -H "Idempotency-Key: $(uuid)" -H 'Content-Type: application/json' \
  -d "{\"amount\":{\"amount_minor\":$MINOR,\"currency\":\"ETB\"}}" >/dev/null
BAL=$(curl -sf $B/v1/wallet/balances -H "$AUTH" | j "['balances'][0]['available']['amount_minor']")
curl -sf -X POST $B/v1/auth/logout -H "$AUTH" -H "Idempotency-Key: $(uuid)" >/dev/null || true
echo "$PHONE now has ETB $(python -c "print(f'{$BAL/100:,.2f}')") (sandbox test money)"
