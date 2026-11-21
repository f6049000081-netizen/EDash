# Backend: sign-in, ledger, payments, top-ups and notifications

This is the Go service behind the auth, profile, wallet and payment endpoints in `../api/openapi.yaml`. It runs against `../db/schema.sql`.

```
cmd/api/              HTTP server entry point
cmd/recon/            imports a partner's daily statement and prints the reconciliation report
internal/identity/    SMS codes, device binding, tokens, sessions, PIN set/change/reset
internal/money/       integer minor-unit money, basis-point maths
internal/ledger/      double-entry posting, balances, account rules
internal/payments/    intents, QR resolution, risk rules, confirm flow, partner top-ups, sandbox top-up
internal/rails/telebirr/  telebirr merchant API: signing, checkout, queryOrder, notification checks
internal/recon/       statement import and matching
internal/outbox/      outbox relay: publishes events in order to Kafka, or in-process without Kafka
internal/notify/      notifications from events, the in-app list, push tokens, Firebase (FCM) sending
internal/platform/    principal, PIN hashing and lockout, idempotency middleware, outbox, problem errors
internal/httpapi/     routes and JSON mapping
```

## How sign-in works

1. **Ask for a code.** `POST /v1/auth/otp` sends a 6-digit code by SMS. The answer is identical whether or not the number is registered. For `pin_reset` and `device_bind` to an unknown number, nothing is sent.
   - Codes are stored only as an HMAC under `OTP_HMAC_KEY`, so a leaked table can't be brute-forced offline.
   - Each code is valid for 5 minutes and dies after 5 wrong guesses.
   - Per number: one code every 30 seconds and 5 per hour, enforced under an advisory lock so parallel requests can't slip through.
   - Per IP: 60 an hour. This is generous because mobile carriers put many users behind one address.
   - Codes go only to allowed country prefixes, which limits SMS-pumping fraud.
2. **Verify it.** `POST /v1/auth/otp/verify`:
   - Creates the user on first sign-in, as `pending` until a PIN is set.
   - Registers or refreshes the device.
   - Returns an access token and a refresh token.
   - Writes an `identity.signed_in` event (with `new_device`) so notifications can warn about new devices.
3. **Use the access token.** It's an Ed25519-signed JWT valid for 15 minutes. Every request also checks that the session is still live in the database, so logout, PIN reset and device removal take effect immediately.
4. **Refresh.** `POST /v1/auth/token/refresh` rotates the refresh token. If an already-rotated token turns up again, someone has copied it, so the whole session family is revoked and an `identity.refresh_token_reused` event is written.
5. **PIN.** `PUT /v1/auth/pin`:
   - Sets the first PIN, which makes the user `active`.
   - Changes it, given the current PIN. Wrong guesses count toward the same 5-try lockout as payments.
   - Resets it from a `pin_reset` session, once, within 15 minutes. A reset clears the lockout and signs out every other session.
   - Rejects easy PINs: repeated digits, runs, repeated halves and the end of the user's phone number.

**SIM-swap guard.** Anyone who takes over a phone number can receive the reset code. So for 24 hours after a PIN reset, every outgoing payment is held for review (`RECENT_PIN_RESET` in the risk rules) instead of executing.

## How a payment works

1. **Create the intent.** `POST /v1/payment-intents` checks the payee and amount and returns the confirm screen data. No money moves at this step.
2. **Confirm it.** `POST /v1/payment-intents/{id}/confirm` with the user's PIN runs these steps in one database transaction:
   - Check the PIN. Failed attempts are committed separately, and five wrong attempts lock the PIN for 30 minutes.
   - Lock the intent row.
   - Run the risk rules and record the decision.
   - Build a balanced journal entry.
   - Lock the balance rows in account-id order. This ordering stops opposing transfers from deadlocking.
   - Post the entry.
   - Mark the intent and write a `payment.*` event to the outbox.
3. **If funds are short,** the ledger write is rolled back to a savepoint. The intent is still recorded as `failed` / `insufficient_funds`, and the API response carries `charged: false`.

QR payments split the amount three ways: debit the payer, credit the merchant settlement account with the amount minus commission, and credit commission revenue. Commission comes from the merchant's plan, or 0.5% if it has none.

## KYC limits

`identity.kyc_limits` sets three amounts per KYC level and currency: the maximum wallet balance, and the most a user may send per day and per calendar month. Days and months run on Ethiopian time.
- **Sending:** checked when a payment is created, so the user hears about it before entering a PIN, and again when it's confirmed. At confirmation the payer's row is locked, so parallel payments are counted one at a time. Payments held for review count toward the limit, because they may still go through.
- **Receiving:** the recipient's balance limit is checked after posting, while their balance row is locked. A transfer that would take them over is undone and fails `limit_exceeded`, and the payer is not charged.
- **Top-ups:** refused up front if the balance plus all unfinished top-ups would exceed the limit. A top-up that is already paid at telebirr but no longer fits can't be refused, so it is held for review instead of credited.
- **Not configured yet:** compliance hasn't supplied values, so a missing row means no limit. `Service.LimitsRequired` turns that into `kyc_required` (fail closed). Production must enable it once the table is filled.
- `GET /v1/me` reports `limits`: the balance limit and what's left to send today and this month. It is null while nothing is configured.

## Activity and profile

- **`GET /v1/activity`:** what the user sent, received and topped up, newest first.
  - Each item has a counterparty name. For a person with no name set, it shows the last four phone digits.
  - Status is `pending`, `completed`, `failed` or `refunded`. Outgoing items also say whether money was charged.
  - Payments never confirmed, or cancelled, are left out.
  - Paging uses an opaque cursor over (created_at, id), with two index-backed queries merged.
- **`PATCH /v1/me`:** display name (whitespace collapsed, up to 60 characters, no control characters), email (unique regardless of case; empty removes it) and language.

## Notifications

- **Outbox relay** (`internal/outbox`). Services write events into `platform.outbox` in the same transaction as the change. The relay reads unpublished rows in id order, publishes them and marks the delivered prefix. A failure part-way through leaves the rest for the next round, so order is kept and nothing is lost. Delivery is at least once.
  - With `KAFKA_BROKERS` set, it publishes to Kafka (keyed by aggregate, so one payment's events stay in order) and the notifications service consumes as group `notify`, committing offsets only after handling.
  - Without it (the dev server), events go straight to the notifications service in the same process.
  - A PostgreSQL advisory lock keeps it to one relay at a time across API instances.
- **What creates a notification** (`internal/notify`):
  - The payee of a successful transfer: "Money received".
  - The payer, only when the outcome came after they left the confirm screen: a top-up decided by the partner, or a payment that was `processing` or `held_for_review`. Results shown on screen aren't repeated.
  - Security alerts: sign-in on a new device (not the account's first), PIN reset, PIN changed, and a session ended because a refresh token was reused.
- **Text** is written in the user's language when the event happens: English and Amharic, with Afaan Oromo and Tigrinya falling back to English until translated. Amounts read "ETB 1,250.00".
- **Repeats are harmless:** one notification per (user, outbox event, kind).
- **Endpoints:** `GET /v1/notifications` (cursor paging, unread count), `POST /v1/notifications/read`, `PUT` and `DELETE /v1/devices/current/push-token`.
- **Push** goes through Firebase Cloud Messaging (HTTP v1) and is off until `FCM_SERVICE_ACCOUNT_FILE` points at a Firebase service-account key. Without it, notifications show in the app only. Pushes go only to devices that still have a live session, so a device signed out by a PIN reset stops getting them; signing out removes the token, and tokens Firebase reports as gone are deleted.

## How a telebirr top-up works

1. **Start it.** `POST /v1/topups` with `{"rail": "telebirr", "amount": ...}`:
   - Records a `processing` intent. Nothing is credited yet.
   - Creates a telebirr order (`preOrder`) whose `merch_order_id` is the intent id.
   - Returns a signed `checkout_url` for the customer to pay in telebirr.
2. **The customer pays in telebirr.** Money moves inside telebirr, from the customer's telebirr wallet to our merchant account. We never touch it directly.
3. **telebirr notifies us** at `POST /v1/webhooks/rails/telebirr`.
   - The RSA signature is checked with telebirr's public key, and so is that the notice is addressed to our merchant app and code.
   - Passing those checks only triggers a `queryOrder` call to telebirr, and that answer decides the outcome. A forged or replayed notification can't credit anything.
   - If telebirr can't be reached, the webhook answers 503 so telebirr retries.
4. **Settle.** Telebirr's answer is applied in one transaction:
   - **Paid in full:** debit the telebirr clearing account, credit the user's wallet, and record telebirr's `payment_order_id` as `partner_ref`.
   - **Paid a different amount, or a `payment_order_id` that already funded another top-up:** held for review, and nothing is credited.
   - **Declined or cancelled:** failed.
   - Settling is idempotent: 10 parallel notifications still credit once.
5. **Sweep.** Every minute, top-ups still processing after 2 minutes are checked with `queryOrder`, in case a notification was lost. Orders unpaid after 2 h 15 m fail as `expired`. If money still arrives after that, reconciliation flags it.

Guards: ETB only, from ETB 1 to ETB 100,000 per top-up until KYC limits are set, and at most 5 unfinished top-ups per user.

**Daily reconciliation.** `recon -rail telebirr -date 2026-11-20 -file statement.csv` imports the partner's statement and matches every line by `partner_ref`. Each line comes out as:
- `matched`
- `amount_mismatch`
- `unmatched`: our side isn't succeeded.
- `missing_internal`: the partner has a payment we have no record of.

It also lists our succeeded top-ups that are missing from the statement. The run exits with status 2 when there are exceptions, so a scheduler can alert on it. The input is a normalised CSV (`partner_ref,amount,currency,occurred_at`). A small mapper from telebirr's own statement layout is still needed once we have a real file.

### telebirr details still to confirm

No official telebirr specification was available, so the protocol follows public open-source integrations:
- [telebirr-js](https://github.com/MelakuDemeke/telebirr-js)
- [GolangTelebirrIntegration](https://pkg.go.dev/github.com/Robotech-Org/GolangTelebirrIntegration)
- [TapBirr](https://github.com/tsenatz/TapBirr/pull/3)

Before going live, check these against the spec and test keys that come with the merchant agreement:
- **Signing:** RSA-PSS with SHA-256 and a 32-byte salt is the default, with `TELEBIRR_SIGN_PADDING=pkcs1` as the fallback. The signed string flattens `biz_content` into the top-level fields, sorts keys and joins `k=v` with `&`. Confirm both.
- **Order fields:** `TELEBIRR_TRADE_TYPE` (default `Checkout`), `TELEBIRR_PAYEE_TYPE` (default `5000`) and `TELEBIRR_BUSINESS_TYPE` (sent only if set). The sources disagree on these.
- **Status values:** we credit only on `order_status` `PAY_SUCCESS`. `PAY_FAILED`, `PAY_CANCEL` and `ORDER_CLOSED` fail the top-up. Anything else stays pending and is checked again.
- **Webhook reply:** what telebirr expects back from the notify URL. We answer HTTP 200 with `{"success": true}`.
- **Gateway and checkout URLs:** different sources give different hosts.
- **In-app checkout:** opening telebirr's own app instead of the web page isn't built yet.

## What's real and what's sandbox

- **Real:**
  - Sign-in, tokens, sessions and PIN management.
  - The ledger, balance rules and concurrency control.
  - PIN lockout, idempotency, risk holds and outbox events.
  - Outbox relay (Kafka or in-process) and in-app notifications. Push to phones needs a Firebase project.
  - telebirr top-ups, notification checks and reconciliation. These are switched off until telebirr merchant credentials are configured (`TELEBIRR_*`; see `cmd/api/main.go`). Without them, `POST /v1/topups` answers `rail_unavailable`. Setting the credentials from telebirr's developer portal connects to telebirr's own test environment. Nothing is simulated on our side.
- **Sandbox only** (`APP_ENV=dev`, the only mode the server starts in for now):
  - **SMS:** codes are written to the server log instead of being sent.
  - **Device attestation:** simulated. Any non-empty `attestation` string is accepted and used as the device's identity, so send the same string each time from the same "device".
  - **Top-ups:** `POST /v1/sandbox/topups` credits a wallet as if a partner rail had delivered the money.
- **Other rails:** bank, card and remittance rails return `rail_unavailable` until licensed partner adapters exist.

## Run the tests

You need Docker. Go doesn't have to be installed locally.

```sh
docker network create g1net
docker run -d --rm --name g1db --network g1net -e POSTGRES_PASSWORD=test postgres:16.4
docker exec -i g1db psql -U postgres -v ON_ERROR_STOP=1 < ../db/schema.sql

docker run --rm --network g1net -v "$PWD:/src" -w /src \
  -e TEST_DATABASE_URL=postgres://postgres:test@g1db:5432/postgres \
  golang:1.23.4 go test -race ./...
```

The integration tests cover:
- Sign-up and sign-in:
  - Code reuse, expiry, and 5 wrong guesses.
  - The resend and hourly throttles, including 10 parallel requests (exactly one gets a code).
  - 10 parallel verifies of one code (exactly one session).
  - Unknown-number resets, and phone and country validation.
- Tokens:
  - Tampering, `alg: none`, the wrong key, expiry and key rotation.
  - Refresh rotation, reuse detection revoking the family, and logout.
- PIN: weak PINs; changing it with the right, wrong or missing current PIN; reset after lockout; the reset signing out other devices; the reset allowance being single use and expiring.
- End to end over HTTP: sign up two users, set PINs, top up, pay. Then reset a PIN and check the next payment is held.
- telebirr client, against a test-only stand-in server (`internal/rails/telebirr/telebirrtest`) that checks our request signatures:
  - Signed-string rules, both paddings, and four key formats.
  - Token caching, and refresh after revocation.
  - A checkout link whose signature survives URL decoding.
  - Order status mapping.
  - Notifications: valid; the `transId` quirk; the `data` envelope; and rejection of tampered, unsigned, wrongly signed and non-JSON bodies.
- Top-ups:
  - Credit only after the partner confirms, and once across 11 settles.
  - Amount mismatch held.
  - Declined and expired top-ups fail, and a late success can't revive an expired one.
  - One payment reference can't fund two top-ups.
  - Limits, the pending-top-up cap, checkout failure, and the sweep.
- telebirr top-up over HTTP: a forged notification gets 401 and credits nothing; the genuine one credits once across 3 deliveries; telebirr down gets 503 and recovers on retry.
- Reconciliation: one line of each kind, our side missing from the statement, re-import refused, duplicate references refused, and CSV parsing.
- Limits (KYC level L1 is reserved for these tests):
  - Daily limit at create and confirm. 8 parallel confirms with room for 4: exactly 4 go through. This test fails if the payer lock is removed.
  - Monthly limit; the recipient's balance limit leaves the payer uncharged; top-up balance limit, both up front and at settlement; `kyc_required` when limits are mandatory but missing.
- Activity: item types, titles, statuses and `charged`; unconfirmed payments hidden; the payee's view; paging one item at a time; bad cursor and filter.
- Profile: whitespace clean-up, partial updates, case-insensitive email uniqueness, validation, and a 60-character Ge'ez name.
- P2P transfers and replayed confirms.
- Insufficient funds.
- The QR commission split.
- 25 concurrent payments against a balance that covers only 10 of them.
- 40 opposing transfers between two wallets, checking for deadlock.
- PIN lockout.
- Large-payment holds.
- Access to other users' intents.
- Refusal of external rails.
- HTTP idempotency.

Every balance check also confirms that the cached balance equals the sum of postings.

## Run the server

```sh
docker build -t g1-api:dev .
docker run --rm --network g1net -p 8080:8080 -e APP_ENV=dev \
  -e DATABASE_URL=postgres://postgres:test@g1db:5432/postgres g1-api:dev
```

Without `AUTH_SIGNING_KEY` and `OTP_HMAC_KEY`, dev mode generates keys at startup, so sessions end when the server restarts. To keep them, generate each key with `openssl rand -base64 32`. See `cmd/api/main.go` for every setting.

Sign in with curl:

```sh
curl -X POST localhost:8080/v1/auth/otp -H 'Content-Type: application/json' \
  -d '{"phone":"+251911234567","purpose":"signup"}'
# the code appears in the server log as "dev sms (not sent)"
curl -X POST localhost:8080/v1/auth/otp/verify -H 'Content-Type: application/json' \
  -d '{"challenge_id":"<id>","code":"<code>","device":{"platform":"android","attestation":"my-test-phone"}}'
# then send "Authorization: Bearer <access_token>" on every other request
```

## Dev server for the app

- **Start it:** `./run-dev.sh` builds the API and runs it in the `g1api` container on port 8080. It uses the `g1dev` database in `g1db`, separate from the test database.
- **Settings:** they come from `backend/.env`, which git ignores. It holds `APP_ENV=dev`, `DATABASE_URL=postgres://postgres:test@g1db:5432/g1dev` and two random base64 32-byte keys, `AUTH_SIGNING_KEY` and `OTP_HMAC_KEY`. Keeping the keys there means sessions survive restarts.
- **First time:** create the database with `CREATE DATABASE g1dev`, then load `db/schema.sql` into it.
- **Sign-in codes:** they aren't sent in dev. Read them with `docker logs -f g1api 2>&1 | grep "dev sms"`. The dev `.env` also sets `OTP_RESEND_SECONDS=60` and `OTP_HOURLY_MAX=1000`, so testers aren't locked out by the production throttle (5 codes per number per hour).
- **Kafka (optional):** to run the relay through Kafka instead of in-process, start a broker on `g1net` (see `internal/outbox/relay_test.go` for a single-node `apache/kafka-native:3.8.0` setup) and add `KAFKA_BROKERS=g1kafka:9092` to `.env`. The Kafka test runs when `TEST_KAFKA_BROKERS` is set.
- **Test money:** `./dev-topup.sh +2519XXXXXXXX 500 ["Name"]` adds sandbox money to that number, creating the account if needed. The money is recorded under its own `sandbox` source and shows in activity as "Test money (sandbox)", never as a real provider. The endpoint exists only on the dev server.
- **Phones:** a phone reaches the API at the PC's address on the local network, so build the app with that address in `API_BASE_URL`. On a network Windows marks as Public, inbound port 8080 may need a firewall rule.

## Next

- **Real providers for production:**
  - An SMS aggregator: Ethio Telecom, Safaricom Ethiopia, and an international route.
  - Play Integrity and App Attest verifiers.
  - Until these exist, the server won't start outside dev.
- **DPoP:** bind access tokens to the device key, alongside biometric confirmation.
- **Shared rate limiting:** the per-IP limiter lives in memory, so it should move to Redis once there's more than one API instance.
- **Localisation:** Afaan Oromo and Tigrinya SMS text, plus a native-speaker review of the Amharic.
- **Fraud review** of the allowed country prefixes. `+1` also reaches Caribbean numbers that SMS-pumping fraud often targets.
- **Limit values:** compliance fills `identity.kyc_limits`; then switch on `LimitsRequired`.
- **Payee lookup throttling:** `payee_phone` shows the recipient's verified name before the PIN (as the design requires), which also lets someone check which numbers have accounts. Rate-limit lookups per user and per device, and flag accounts that create many unconfirmed intents.
- **Push to phones:** create the Firebase project, set `FCM_SERVICE_ACCOUNT_FILE`, and add the Firebase SDK and `google-services.json` to the app so it registers its token.
- **Notification translations:** Afaan Oromo and Tigrinya text, and a native-speaker check of the Amharic.
- **telebirr go-live:**
  - Confirm the points under "telebirr details still to confirm" with the merchant agreement.
  - Map telebirr's statement file to the recon CSV and schedule the daily run.
  - Add in-app checkout.
  - Show reconciliation exceptions in the admin console.
- **Biometric confirmation:** verify device-key signatures.
- **Merchant payout batching**, and orders and bills in the activity feed once those modules exist.
