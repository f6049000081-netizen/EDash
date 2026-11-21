# Gebeya One

Gebeya One is a wallet-centred super app for Ethiopia and the diaspora. This repository holds the Phase 1 foundations: the data model and the API contract.

## Design work

| Deliverable | Link |
| --- | --- |
| Competitive analysis and product architecture | https://claude.ai/artifact/R2R9dBFUKPtoC2wysJq9dr |
| Design system | https://claude.ai/artifact/Bb7HFDYAUe4mJ4BDBpdbKi |
| Consumer app prototype | https://claude.ai/artifact/NUudyMKSsTz9mosVnjL7pN |
| Merchant and admin consoles | https://claude.ai/artifact/Jvc2BSzXZH9YiDYFhCYsG4 |

## What's here

```
db/schema.sql      PostgreSQL 16 schema: 13 domain schemas, double-entry ledger, RBAC, audit log
api/openapi.yaml   OpenAPI 3.1 contract for Phase 1 (auth, wallet, payments, bills, merchants, admin, webhooks)
backend/           Go service: sign-in, ledger, payments, telebirr top-ups, reconciliation, outbox relay and notifications (see backend/README.md)
app/               Flutter consumer app: sign-in, wallet, send, QR pay, top-up, activity, notifications, profile (see app/README.md)
```

## Data model rules

- **Money** is stored as `BIGINT` minor units with an ISO currency code (1 ETB = 100 santim), and is never a float. FX rates are `NUMERIC(18,8)` and travel over the API as decimal strings.
- **The ledger is double-entry and append-only.** A deferred constraint trigger rejects any journal entry whose debits and credits differ in any currency. Updates and deletes raise an error, so corrections are posted as reversal entries.
- **One journal entry per business event.** `(source_type, source_id, kind)` is unique, so a retried payment cannot post twice. The API's `Idempotency-Key` gives the same guarantee at the request level.
- **We don't hold customer funds.** `user_wallet` accounts mirror balances held by licensed partners. The `recon` schema matches each partner's daily statement line by line against our records.
- **Maker-checker is enforced in the database.** An approval cannot be decided by the person who requested it, and staff cannot grant roles to themselves.
- **The audit log is append-only** and hash-chained (`prev_hash`, `row_hash`), so changing any row breaks every hash after it.
- **Events use a transactional outbox.** State changes and their events are written in one transaction, and a relay then publishes them to Kafka.
- **Each domain owns its own schema** (`identity`, `ledger`, `payments`, …). Services read other domains' data through the API or through events, never by joining across schemas.
- **High-volume tables are partitioned by month:** `ledger.postings` and `admin.audit_log`.

## Checks run

- The schema loads cleanly into a fresh `postgres:16.4` container.
- Ledger and approval rules were tested by hand: a balanced entry commits, an entry that is unbalanced by one santim is rejected, a duplicate business event is rejected, updates to postings are blocked, and self-approval is blocked.
- `npx @redocly/cli@1.25.0 lint api/openapi.yaml`: valid, with 24 style warnings (missing tag descriptions and a few operations without a 4XX response).

To check the schema again:

```sh
docker run -d --rm --name g1 -e POSTGRES_PASSWORD=test postgres:16.4
docker exec -i g1 psql -U postgres -v ON_ERROR_STOP=1 < db/schema.sql
docker stop g1
```

## Open items

- KYC limits per level (`identity.kyc_limits`) need values from compliance, based on current National Bank of Ethiopia directives.
- The telebirr adapter follows public integrations because no official spec was available. Its signing, status values and URLs must be confirmed against the merchant agreement (listed in backend/README.md). EthSwitch, card and remittance adapters depend on their own agreements.
- Partitions are created only for Nov and Dec 2026. A scheduled job (pg_partman or a cron task) should create future months ahead of time.
- Personal data columns such as `legal_name` are marked for application-level envelope encryption. The key management service still has to be chosen.
