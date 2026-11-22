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
internal/merchants/   merchant sign-up (KYB), approval, shop QR, today's summary, payments received
internal/payouts/     merchant payouts: batching, payout accounts, sending through a licensed partner
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

## Device keys: DPoP and biometric confirmation

- **DPoP** (RFC 9449, ES256 only, since that's what Android Keystore and the iOS Secure Enclave hold). The app keeps a key pair in secure hardware and signs a short proof for every request (method, URL, time, a one-time id, and the access token's hash).
  - A sign-in carrying a proof binds the session to that key (`token_type: DPoP`, `cnf.jkt` in the token). From then on the token is useless without the key: as a bearer token, with another key, with a replayed or stale proof, or for another URL, it gets 401.
  - Refreshing a bound session needs a proof from the same key. A refresh token stolen alone gets `invalid_dpop_proof`, and it doesn't sign the owner out.
  - Sessions from app versions without DPoP keep working as bearer sessions; the first refresh with a proof binds them. Set `DPOP_REQUIRED=true` once every app in use sends proofs.
  - Proof `htu` is checked against `PUBLIC_BASE_URL` + path, or against the request's own host when that isn't set. Set it when the API sits behind a proxy.
  - Used proofs are kept in `identity.dpop_proofs` until they expire, so each is accepted once.
- **Biometric confirmation.** A second key in secure hardware that only works after the phone's fingerprint or face check.
  - Registered with `PUT /v1/devices/current/biometric-key` and the **PIN**. Sign-in no longer accepts a biometric key, because an SMS code alone (a SIM swap) must never set up a way to pay without the PIN.
  - Confirming: the intent carries `confirm_challenge` (intent id, amount, fee and payee); the app signs those exact bytes and sends `biometric_signature`. The key works only for the device it was registered on.
  - A PIN reset clears biometric keys on every device. Turning it on sends a security notification to all the user's devices.
  - Refused signatures (`biometric_not_enrolled`, `biometric_failed`) tell the app to ask for the PIN.

## Merchants

- **Applying** (`POST /v1/merchants`). A signed-in user with a PIN applies with the shop name, registered name, TIN (10 digits), optional trade licence, category and city. The application goes to `kyb_review` and the applicant becomes its `owner`.
  - One application under review per person. Sending the same one again returns it.
  - A TIN can only be registered once, unless an earlier application with it was rejected.
- **Approval** is done by staff in the operations console (below). `Approve` gives the shop:
  - an 8-digit merchant number (random, last digit a Luhn check digit)
  - a static EMVCo merchant-presented QR. The merchant account template (tag 26) uses the GUID `one.gebeya.pay`. Names are folded to plain Latin for tag 59, and Ge'ez names also go in tag 64. The CRC is CRC-16/CCITT-FALSE.
  - Other wallets can parse the QR, but only EDash resolves it until the national interoperable QR scheme registers the template.
  - For local development: `./dev-approve-merchant.sh <merchant id | TIN>`. It runs `cmd/devtool`, which refuses any database that isn't local.
- **Roles** (`merchant.members`):
  - Every member sees the shop, today's sales and the payments received (`GET /v1/merchants/{id}/summary`, `/payments`). That's how a cashier checks a customer has paid.
  - Cashiers don't see the registration details, payouts or the payout account.
  - Only owners change the payout account.
  - Strangers get 404.
- **Payers** appear by first name and last initial ("Abebe K.").
- **"Today"** is the day in Addis Ababa.

## KYB documents

`internal/kyb`: the documents a business sends to be verified.

- **Sending** (`POST /v1/merchants/{id}/documents?kind=…`, owner only). The body is the file itself, at most 10 MB.
  - The server goes by the file's first bytes, not the Content-Type: only JPEG, PNG and PDF are taken.
  - Sending the same file again returns the stored document, so retries are safe. A new document of a kind replaces the earlier ones of that kind.
  - Kinds: `tin_certificate`, `trade_licence`, `owner_id`, `other`. An application needs the TIN certificate and the owner's ID accepted, plus the trade licence when it gave a licence number (`GET …/documents` returns this as `required`).
- **Storage.** Files are encrypted with AES-256-GCM before they leave the server, bound to their object key, then kept in any S3-compatible store.
  - A leaked bucket reveals nothing, and a file moved to another key won't open. The SHA-256 of each file is checked when it's read back.
  - Keys rotate: `DOCS_KEYS=new:…,old:…` encrypts with the first and still reads the others. Each document records the key it used.
  - Settings: `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_REGION`, `DOCS_KEYS` (both servers). Without `S3_ENDPOINT`, document endpoints answer 503 `documents_unavailable` and the console approves without checking documents.
  - **Choosing the production store** is still open: a provider hosted in Ethiopia if data residency rules require it, otherwise a cloud bucket in the nearest region, with versioning and no public access.
- **Review** happens in the operations console (below): staff open each document there, every opening is recorded, and they accept or reject it. A rejection tells the owner what to resend and why.

## Merchant payouts

- **Batching** (`internal/payouts`, run every 5 minutes by the API server). On each merchant's schedule, a payout collects the QR sales not yet paid out: up to midnight Addis Ababa time for `daily`, Monday midnight for `weekly`, everything for `instant`. Balances under ETB 10 carry over.
  - Each sale's ledger entry is recorded in `merchant.payout_items`, with its gross, commission and net. The primary key means a sale is paid out once, even when two runs race.
  - The net moves from the shop's `merchant_settlement` account into its `hold` account, so the money stays reserved for the shop until it really leaves.
- **Sending.** A `payouts.Rail` sends each transfer through one licensed partner. Each attempt has its own request id (`<payout id>-<attempt>`), and a partner must treat a repeat of the same id as the same transfer.
  - A timeout is retried under the same id. A definite temporary failure is retried under a new id, up to 3 attempts. A permanent failure (closed or wrong account) puts the payout on hold.
  - When the partner confirms payment, the net moves from `hold` to the partner's clearing account and the payout is `settled`.
  - **No payout rail exists yet.** Until a licensed partner's credentials are configured, payouts wait as `scheduled` with `last_error: rail_unavailable`. Nothing is simulated.
- **Fraud controls.**
  - Only an owner can change the payout account (`PUT /v1/merchants/{id}/payout-destination`), and only with their PIN.
  - Every owner and accountant is notified of the change, and nothing is paid to a new account for 48 hours.
  - Payouts of a suspended shop are held.
  - Strangers get 404 for a shop's payout endpoints; cashiers get 403.
- **Notifications** to owners and accountants: payout sent, and payout on hold (with the reason).
- **Releasing** a payout held after a partner rejection or repeated failures is done by staff in the operations console, with a second person's approval.
- **Not built yet:** reversing failed payouts, refunds in payout statements, and the web merchant console.

## Operations console

A separate server (`cmd/ops`, port 8090) with server-rendered pages for staff (`internal/opsweb`), the rules in `internal/ops`. Run it only on the company network; the API server doesn't serve these pages.

- **Sign-in** is through the company identity provider (OpenID Connect, code flow with PKCE, state and nonce). The console keeps no passwords.
  - Staff are invited by email. The first sign-in binds the identity provider's subject to the staff row; after that only the subject counts.
  - `OPS_REQUIRED_AMR` (e.g. `hwk`) makes the ID token prove a hardware key was used. The server refuses to start without it outside `APP_ENV=dev`.
  - Sessions: an `__Host-` cookie (HttpOnly, Secure, SameSite=Strict), only its hash stored, 30 minutes idle and 8 hours at most. Signing out ends the session in the database.
  - Every form carries a per-session CSRF token and is refused from another origin. Pages have no scripts, under a strict Content Security Policy.
- **Roles and grants** come from `admin.role_permissions`: `full` acts alone, `approve` needs a second person, `view` only reads.
  - Approving or rejecting applications: `merchants.approve`. Suspending or reinstating: `accounts.freeze`. Releasing a held payout: `payouts.release`.
  - Risk cases: `risk.cases` (take, note, decline, resolve, escalate), `payments.release` (release a held payment) and `accounts.freeze` (freeze a customer).
  - Staff: `staff.manage`, which only super admins have, as `approve`: every staff change needs a second super admin.
- **Maker-checker.** With an `approve` grant, an action becomes a request in `admin.approvals` with a written reason (10+ characters). It is tried in a savepoint first, so a request that can't succeed is refused at once.
  - Someone else with the grant decides it, with their own note. Nobody decides their own request; the database refuses it too.
  - Requests lapse after 72 hours. The same request can't be pending twice.
- **Actions.**
  - Approve: as in Merchants above, once the required documents are accepted. Reject: the application is closed and the TIN can be used again.
  - Documents: staff who can approve applications accept or reject each one directly; the approval is what needs the second person.
  - Suspend: the QR stops working and payouts are held (`merchant_suspended`); the money stays reserved. Reinstate: those payouts are scheduled again.
  - Release: only payouts held for a partner rejection or repeated failures, and only to a current payout account. The attempt count starts again.
  - The owner is notified of a rejection, suspension or reinstatement, with the reason staff wrote.
- **Audit log.** Every attempt is recorded, with role, IP and device: sign-ins, document openings, refusals (`denied`), and allowed actions the rules stopped, such as approving with documents missing (`failed`, with the reason). Rows are hash-chained and can't be updated or deleted; the audit page checks the whole chain.
- **Risk cases.** A payment held by the risk rules (or a top-up held for a mismatch) opens a case, numbered `FR-20001` onwards. Further holds for the same customer join their open case: the score is the highest, the amount the sum.
  - Due in 4 hours when the score is 80+ or the amount ETB 50,000+, otherwise 24. Late cases are marked in the list and counted on the overview.
  - An analyst takes the case, writes notes, and decides each held payment. **Release** sends it now, with a second person's approval, as if it had just been confirmed: if the money is no longer there it fails with `insufficient_funds`. **Decline** ends it (`declined_after_review`); the payer keeps their money and is only told it didn't go through.
  - Held top-ups can't be decided here yet: the customer already paid the partner, so ending one needs the partner's refund flow.
  - **Freeze** (second person) stops the customer sending or paying; they're told their account is on hold, not why. Unfreeze the same way.
  - **Resolve** with an outcome, once no payment is still held. **Escalate** hands the case to AML compliance, who decide whether to report to the Financial Intelligence Service; the customer must not be told.
  - The case page shows the customer (masked phone, balance, 30-day activity), the payments with the rules that fired, pending requests, and the history of the case, its payments and its customer.
- **Staff management.** Invite (name, company email, roles), change roles, suspend (signed out at once) and reinstate, each needing a second super admin. The request carries the details, so the approver sees exactly what will change. Nobody can request or approve a change to their own access, and the last active super admin can't be removed.
- **Not built yet:** reversing failed payouts, refunds for held top-ups, and a case view for merchants.

### Run it locally

```sh
./run-ops-dev.sh   # http://localhost:8090/ops/, against the g1dev database
```

The first run makes `backend/.dex/` (git-ignored): a Dex config standing in for the company identity provider, with three staff accounts and random passwords in `.dex/passwords.txt`.

- `tsion.admin@gebeya.dev`: super admin, acts alone on most things.
- `selam.admin@gebeya.dev`: a second super admin, to approve Tsion's staff changes (and the other way round).
- `dawit.risk@gebeya.dev`: risk analyst and finance officer. His suspensions, payout and payment releases and account freezes need a super admin's approval.

To get a risk case, pay more than ETB 50,000 from a test account (`./dev-topup.sh` first); the payment is held and the case appears under Risk cases.

Dex has no MFA, so the dev console doesn't ask for a hardware key.

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

## Commerce: shops, products and orders

`internal/commerce`, with the routes in `internal/httpapi/commerce.go`. Customers order from approved shops for pickup or delivery and pay from their wallet like any other payment, but the shop is only paid once the customer has the order.

- **Shops.** An approved merchant sets its ordering settings with `PUT /v1/merchants/{id}/shop`: open or not, pickup and/or delivery, delivery fee and area, and a short "about". A shop can't open without at least one active product. `GET /v1/shops` lists approved, active shops for customers.
- **Products** (`POST` and `PUT /v1/merchants/{id}/products`). Each has a title, an optional description and shelf name, a price, and an optional stock count (`null` means the shop doesn't count it). Status is `active`, `hidden` or `archived`. Owners and managers change them.
- **Placing an order** (`POST /v1/orders`, with an `Idempotency-Key`). The server prices the order from the catalog, never from the app, and reserves the stock. It then creates a payment intent of kind `order`, which the customer confirms like any payment (PIN or biometric, with the same limits, risk rules and holds). A shop's own staff can't order from it.
- **Escrow.** The confirmed payment moves the money from the customer's wallet into the platform's `order_escrow` account.
  - **On handover** it moves from escrow to the shop's `merchant_settlement` account, minus commission, and the normal payout batching picks it up. Commission uses the shop's `qr` plan for now, or the default 50 basis points.
  - **On any cancellation after payment** it goes straight back to the customer's wallet (entry kind `order_refund`), the payment intent becomes `reversed`, and the stock is returned.
- **Order status:** `pending_payment` → `placed` → `accepted` → `ready` → `on_the_way` (delivery only) → `delivered`. An order can be `cancelled` at any point before `delivered`.
  - **The customer** can cancel until the shop accepts. They can also confirm they have the order (`POST /v1/orders/{id}/received`) once it's accepted.
  - **The shop** (`POST /v1/merchants/{id}/orders/{order}/{action}`) can `accept`, mark it `ready` or `on_the_way`, `hand_over` it with the customer's code, or `cancel` it with a note the customer sees. Owners, managers and cashiers can do this; accountants can only view.
- **Handover code.** A random four-digit code that only the customer sees, and only once the order is paid. It's never in notifications or in the shop's view, and it's compared in constant time. After 5 wrong tries the code stops working, and the customer confirms with "I have my order" instead.
- **Timeouts** (`RunOnce`, every minute in the API server): orders not paid within 30 minutes are cancelled, and paid orders the shop hasn't accepted within 20 minutes are cancelled and refunded.
- **Product photos** (`internal/photos`, `commerce/photos.go`). Owners and managers add one photo per product, from the app (camera or gallery) or the web business console (`PUT`/`DELETE /v1/merchants/{id}/products/{product}/photo`, raw JPEG, PNG or WebP up to 10 MB).
  - Every upload is checked by its content and drawn again as two JPEGs (1200 px and a 320 px thumbnail). Nothing from the original file is kept: no GPS position or camera details, and nothing that isn't a picture. Pictures over 40 megapixels are refused before they're decoded, and so are ones under 200 px a side.
  - They're kept in the same S3-compatible store as KYB documents, in their own bucket (`S3_PHOTOS_BUCKET`, default `product-photos`), unencrypted because they're public shop content. Without `S3_ENDPOINT`, uploads answer `photos_unavailable`.
  - `GET /v1/products/{product}/photos/{photo}` serves only a product's current photo, without sign-in (the app's image loader can't sign requests), cached for a year: each new photo gets a new ID, and the old files are deleted.
  - The web business console shows them through its own origin (`/business/products/{id}/photo/...`), only for its own shop's products.
  - **Checked live** on 2026-11-22 against SeaweedFS: a 1600×1200 phone photo with GPS data in its EXIF came back as 1200×900 and 320×240 JPEGs without it; a customer couldn't change it; a PDF was refused; the photo showed on the shop page and in the console; replaced and removed photos stopped being served.
- **Accepted orders that are never handed over** (`FlagStalled`, also every minute). Nothing is decided automatically, because either outcome moves money and could be wrong: the customer's money stays in escrow until a person decides.
  - **After a day** (`RemindAfter`, 24 h after accepting) the shop and the customer are reminded (`order.overdue`). The shop is asked to hand it over or cancel it; the customer is asked to tap "I have my order" if they have it. Orders show `overdue: true`.
  - **After three days** (`ReviewAfter`, 72 h) the order goes to operations (`order.review`, `in_review: true`), and both sides are told our team will settle it.
  - **In the operations console** (Unfinished orders, permission `orders.settle`): staff call both sides, then ask to refund the customer (the order is cancelled as `not_completed` and the stock returns) or to pay the shop (the order is completed as if handed over). Either needs a second person: support agents, finance officers and super admins can ask and approve; risk analysts and merchant operations can look. Both sides are notified of the outcome.
  - Until staff decide, the shop can still hand the order over with the code and the customer can still confirm it; a request for an order finished in the meantime fails when it's approved.
  - **Checked live** on 2026-11-22 on the dev server: an accepted order backdated a day was marked overdue within a minute, then backdated three days went to review, with the reminder and review notifications in Amharic for both the customer and the shop.
- **Locking.** The payment intent is always locked before its order, the same order payment confirmation uses, so a cancellation can't deadlock with a payment in flight.
- **Privacy.** The shop sees the customer's name. The full phone number is shown only on delivery orders in progress, and is masked otherwise.
- **Events and notifications.** The outbox carries `order.placed`, `order.accepted`, `order.ready`, `order.on_the_way`, `order.delivered` and `order.cancelled`. The notifications service turns them into English and Amharic notices for the customer and for the shop's staff. Orders appear in the activity feed (`?filter=orders`); refunded ones show as `refunded`.
- **Held payments.** When risk holds an order's payment, the order waits as `pending_payment`. If staff release it in the operations console, the order is placed then.
- **Checked live** on 2026-11-22 against the dev server: shop set-up, order, PIN payment, accept, ready, a wrong code refused, handover with the right code, ETB 358.20 pending payout from a ETB 360 order, then a second order cancelled and refunded with its stock returned.
- **Not built yet:**
  - Product photos and delivery time slots.
  - Refunds after handover (returns and disputes).
  - Couriers and delivery tracking.
  - A separate commission plan for orders.
  - Orders in the operations console.
  - Accepted orders that never finish: today nothing closes them, so a timeout or a staff action is needed.

## Web business console

`cmd/business` (port 8070) and `internal/bizweb`: a shop's orders, products, storefront, payments and payouts on a computer, following the merchant console design (Home, Orders, Products, Storefront, Payments and payouts). Pages are rendered on the server, have no scripts, and are in English and Amharic (Afaan Oromo and Tigrinya fall back to English). The console follows the person's app language, and they can switch it at the bottom of the menu.

- **Signing in never involves a password, SMS code or PIN in the browser.**
  - The browser shows a one-time code (8 characters, `ABCD-EFGH`), and checks back by itself every 3 seconds.
  - A member of the shop types the code in the app (Profile → Business → Sign in on a computer). The app shows which browser is asking (from `POST /v1/web-logins/lookup`), and the person approves with their PIN (`POST /v1/web-logins/approve`, with the usual PIN lockout).
  - The browser then gets a session for that person and that shop.
  - Codes work for 5 minutes and once. Only hashes of the code and of the browser's secret are stored, and each network can request at most 20 codes per 10 minutes.
  - The person gets a notification (`identity.web_signed_in`).
- **Sessions** (`merchant.web_sessions`): an `__Host-` cookie that is HttpOnly, Secure and SameSite=Strict. Every form also needs a per-session CSRF token and passes a same-origin check. The Content Security Policy allows no scripts.
  - A session ends after 2 hours without use, after 12 hours at most, on sign-out, or when it's signed out from the app (`GET` and `DELETE /v1/me/web-sessions`).
  - It also ends at once if the person leaves the shop, is frozen, or the shop isn't active.
- **Permissions.** What a session can do is read afresh on every request and checked by the same services the app's API uses: owners and managers change products and the storefront, cashiers handle orders, accountants view. Payouts are shown to owners, managers and accountants. The payout account can still only be changed in the app, by an owner, with their PIN.
- **Orders** refresh every 20 seconds while the list is open. From the list, accept and the next step are one click. Handover (the customer's 4-digit code) and declining or cancelling (with a note to the customer) are on the order page.
- **Run it locally:** `./run-business-dev.sh`, then open http://localhost:8070/business/ and approve the code from the app (the API server from `run-dev.sh` must be running). Production needs `BUSINESS_BASE_URL` on https.
- **Checked live** on 2026-11-22: a code shown in the browser, looked up and approved in the app, then an order accepted, marked ready, a wrong code refused and handed over from the console, the console in Amharic from the owner's app language, the sign-in notification, and the browser signed out from the app.
- **Not built yet:** promotions, customers and reviews, insights, team and roles, and branches from the design. Validation messages from the services are still in English. Before production the console should run behind the same edge protection as the API.

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
- **Document storage:** `run-dev.sh` and `run-ops-dev.sh` call `dev-storage.sh`. It starts SeaweedFS, an S3-compatible store, in the `g1s3` container (files in the `g1s3data` volume). On first run it writes `backend/.s3/` (git-ignored) with random credentials and a document key. The S3 test runs when `TEST_S3_ENDPOINT=g1s3:8333`, `TEST_S3_ACCESS_KEY` and `TEST_S3_SECRET_KEY` are set (values in `.s3/docs.env`).
- **Kafka (optional):** to run the relay through Kafka instead of in-process, start a broker on `g1net` (see `internal/outbox/relay_test.go` for a single-node `apache/kafka-native:3.8.0` setup) and add `KAFKA_BROKERS=g1kafka:9092` to `.env`. The Kafka test runs when `TEST_KAFKA_BROKERS` is set.
- **Test money:** `./dev-topup.sh +2519XXXXXXXX 500 ["Name"]` adds sandbox money to that number, creating the account if needed. The money is recorded under its own `sandbox` source and shows in activity as "Test money (sandbox)", never as a real provider. The endpoint exists only on the dev server.
- **Phones:** a phone reaches the API at the PC's address on the local network, so build the app with that address in `API_BASE_URL`. On a network Windows marks as Public, inbound port 8080 may need a firewall rule.

## Next

- **Real providers for production:**
  - An SMS aggregator: Ethio Telecom, Safaricom Ethiopia, and an international route.
  - Play Integrity and App Attest verifiers.
  - Until these exist, the server won't start outside dev.
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
- **Device keys in the app:** the server side of DPoP and biometric confirmation is done; the app still has to create the hardware keys and send proofs. Then switch on `DPOP_REQUIRED`.
- **Key attestation:** check Android key attestation and App Attest so the server knows the DPoP and biometric keys really live in secure hardware.
- **Payout partner:** connect a licensed payout rail (telebirr B2C, EthSwitch) once credentials exist, and add reversing failed payouts to the operations console.
- **Operations console:** connect the company identity provider with hardware-key MFA; refunds for held top-ups through the partner; an AML compliance view for escalated cases; risk cases for merchants.
- **Document storage for production:** choose the provider (see KYB documents), then set `S3_*` and `DOCS_KEYS` from the secrets manager. Add malware scanning of uploads before staff open them.
- **Bills** in the activity feed, once that module exists.
- **Commerce:** see "Not built yet" under Commerce.
