# Gebeya One: working notes for Claude

Wallet-centred super app for Ethiopia and the diaspora. The original brief is `Master Prompt.docx`. Read `README.md` (project) and `backend/README.md` (service) before changing code; they describe the design and current state in detail.

## Hard rules from the brief

- Financial services must go through properly licensed banking, payment and financial providers. **Never simulate real financial transactions.** Rails without real partner credentials must answer `rail_unavailable`. Test doubles live only in tests (for example `internal/rails/telebirr/telebirrtest`).
- Order of work: competitor analysis and architecture, then design system and hi-fi UI, then implementation. All three design phases are done.
- Languages: Amharic, Afaan Oromo, Tigrinya, English. ETB first, plus multi-currency and diaspora remittance.

## Progress (update this list when a milestone lands)

1. **Design deliverables (claude.ai artifacts).** The links are in `README.md`: competitive analysis and architecture, design system, consumer app prototype, merchant and admin consoles.
2. **Data model.** `db/schema.sql` (PostgreSQL 16).
3. **API contract.** `api/openapi.yaml` (OpenAPI 3.1).
4. **Ledger and payments** (Go): double-entry ledger, P2P and QR payments, risk holds, PIN lockout, idempotency, outbox.
5. **Sign-in service:** SMS codes, device binding, Ed25519 access tokens, rotating refresh tokens, PIN set/change/reset, and a 24 h payment hold after a PIN reset.
6. **telebirr top-ups and daily reconciliation.** Switched off until merchant credentials are set. The wire details still to confirm against the telebirr merchant agreement are listed in `backend/README.md`.
7. **Backend round for the app:** activity feed, KYC limit enforcement (values still to come from compliance) and `PATCH /v1/me`.

8. **Flutter consumer app** in `app/`: sign-in, PIN, home, send, QR pay, telebirr top-up, activity and profile, in four languages. `flutter analyze` is clean and 20 tests pass. Run it with `app/flutterw.sh` (Flutter runs in Docker from the `g1flutter` volume). A debug APK builds with `./flutterw.sh 'flutter build apk --debug ...'` (Android SDK and JDK 17 in the `g1android` volume). Tested on a real Android phone against the dev server (`backend/run-dev.sh`) on 2026-11-21: sign-in, PIN, balance, QR pay and send to a phone number all worked. Test money comes from `backend/dev-topup.sh` (sandbox rail).

9. **Outbox relay and notifications** (2026-11-22): relay to Kafka or in-process (`internal/outbox`), notifications service with am/en text, in-app list and bell, push tokens, Firebase sender (off until a Firebase project exists). Tested on the phone: the bell showed Abebe's notifications.

10. **DPoP and biometric confirmation** (2026-11-22): sessions bound to a hardware key (RFC 9449), payments confirmable with a biometric-gated key registered with the PIN. Android only (`MainActivity.kt`); iOS still uses bearer tokens and the PIN.

11. **Merchant payouts** (2026-11-22): batching of unpaid QR sales into payouts (reserved in the ledger), payout accounts (owner + PIN, 48 h cooling-off, notifications), sending through a `payouts.Rail` (none connected: `rail_unavailable`), retries and holds, payout history API.

12. **Merchant sign-up and Business screens** (2026-11-22): KYB application, approval (`Approve`: merchant number with Luhn digit, EMVCo static QR; dev-only `backend/dev-approve-merchant.sh`), roles, today's summary, payments received; in the app, Profile → Business (apply, shop screen with QR, payouts, payout account with PIN).

13. **Operations console** (2026-11-22): `cmd/ops` on port 8090, `internal/ops` (rules) and `internal/opsweb` (pages). Staff sign in with OpenID Connect (Dex in dev: `backend/run-ops-dev.sh`, passwords in `backend/.dex/passwords.txt`), hardware-key check via `OPS_REQUIRED_AMR`. Roles and grants from the permission matrix, maker-checker requests with notes and a 72 h lapse, hash-chained audit log. Actions: approve, reject, suspend and reinstate merchants, release held payouts. Signed in through Dex on 2026-11-22.

14. **KYB documents** (2026-11-22): `internal/kyb`. Owners send photos from the app (Business screen, `image_picker`), checked by content, encrypted with AES-256-GCM and kept in any S3-compatible store (`S3_*`, `DOCS_KEYS`; SeaweedFS in dev via `backend/dev-storage.sh`). Staff open, accept or reject them in the console; approval waits for the required documents. Allowed-but-refused console actions are audited as `failed`. Checked live on 2026-11-22 against the dev servers, from upload to approval. Still open: the production storage provider and malware scanning.

15. **Risk cases and staff management** (2026-11-22): held payments and top-ups open risk cases (`FR-` numbers, one open case per customer, 4 h or 24 h due time). In the console: take, notes, release a held payment (second person; `payments.ReleaseHeldTx` posts it then), decline, freeze or unfreeze the customer (they're notified), resolve, escalate to AML. Staff page: invite, roles, suspend, reinstate, all needing a second super admin, never on yourself. Dev has a second super admin (`selam.admin`). Checked live on 2026-11-22: held payment, case, Dawit's release request, Tsion's approval, payment succeeded.

**Next on the roadmap:** a web merchant console (orders, products) once commerce exists. Also open: the iOS Secure Enclave side of device keys, key attestation and then `DPOP_REQUIRED=true`, a list of signed-in devices with sign-out, and a licensed payout partner. Waiting on others: a native-speaker review of the Afaan Oromo and Tigrinya text, and a Firebase project for lock-screen pushes. Telebirr go-live work is listed under "Next" in `backend/README.md`.

## Running things (Windows; Go is NOT installed, everything runs in Docker)

Docker Desktop may need starting first: `Start-Process "C:\Program Files\Docker\Docker\Docker Desktop.exe"`.

```sh
# database (container is --rm: data is gone when it stops, and that's fine)
docker network create g1net
docker run -d --rm --name g1db --network g1net -e POSTGRES_PASSWORD=test postgres:16.4
docker exec -i g1db psql -q -U postgres -v ON_ERROR_STOP=1 < db/schema.sql

# tests, from backend/ (Git Bash: MSYS_NO_PATHCONV=1 and $(pwd -W) are needed)
MSYS_NO_PATHCONV=1 docker run --rm --network g1net -v "$(pwd -W):/src" -v g1gomod:/go/pkg/mod \
  -v g1gocache:/root/.cache/go-build -w /src \
  -e TEST_DATABASE_URL=postgres://postgres:test@g1db:5432/postgres golang:1.23.4 \
  sh -c 'gofmt -l . ; go vet ./... && go test -race -count=1 ./...'

# contract lint
npx -y @redocly/cli@1.25.0 lint api/openapi.yaml   # expect "valid", 24 style warnings
```

There is no migration tool yet. Schema changes go straight into `db/schema.sql`, and tests run against a fresh load of it.

## Gotchas seen in this repo

- Python on Windows writes CRLF in text mode, which gofmt flags. Open files with `newline=''`.
- In Bash heredocs, a backslash before a newline can get eaten before Python sees it. For edits containing `\`, write the script to a file first.
- Writing `"\ufeff"` through the Write tool produced a literal BOM, which Go rejects. Use the escape text instead.
- Integration tests share one database across runs. Every test must use fresh random phone numbers, IDs and dates (see the existing helpers).
