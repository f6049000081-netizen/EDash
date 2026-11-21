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

**Next on the roadmap:** merchant payouts. Also open: the iOS Secure Enclave side of device keys, key attestation, then `DPOP_REQUIRED=true`. Waiting on others: a native-speaker review of the Afaan Oromo and Tigrinya text, and a Firebase project for lock-screen pushes. Telebirr go-live work is listed under "Next" in `backend/README.md`.

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
