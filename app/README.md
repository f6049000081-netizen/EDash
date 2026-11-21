# Gebeya One: consumer app (Flutter)

The wallet app for Android and iOS, built on the approved design system and connected to the Go API in `../backend`.

## What's in it

| Screen | What it does | API |
| --- | --- | --- |
| Sign-in | Phone number (Ethiopian numbers in any common form, or `+country` for the diaspora), 6-digit SMS code, language picker | `POST /v1/auth/otp`, `/v1/auth/otp/verify` |
| PIN setup | 6-digit PIN entered twice; the server rejects weak PINs | `PUT /v1/auth/pin` |
| Home | Greeting, balance card (can be hidden), Send / Scan / Add money, five most recent items | `/v1/wallet/balances`, `/v1/activity`, `/v1/me` |
| Send | Recipient's phone and amount, then review (verified name, fee, total), PIN, result | `POST /v1/payment-intents` (`payee_phone`), `/confirm`, `/cancel` |
| Scan to pay | Camera QR scan, merchant's verified name, amount unless the QR fixes it, then the same review/PIN/result | `POST /v1/qr/resolve`, `/v1/payment-intents` |
| Add money | telebirr top-up: opens telebirr's checkout and waits for the server to confirm | `POST /v1/topups`, `GET /v1/payment-intents/{id}` |
| Activity | All / Money / Orders filters, cursor paging, status pills | `GET /v1/activity` |
| Profile | Name, email, language, remaining limits, change PIN, sign out | `GET/PATCH /v1/me`, `PUT /v1/auth/pin` |

Bottom navigation follows the design: Home, Explore, a raised centre **Pay** button, Activity and Profile. Explore says plainly that services will appear as partners join; it shows no placeholder services.

## How it handles money

- **Amounts.** Amounts are integers in minor units (santim), as the API sends them. Doubles are never used. Amounts are shown as `ETB 1,250.00` with tabular figures, and screen readers hear "1250 Ethiopian birr".
- **No simulated money.** The app never credits a wallet itself.
  - A top-up only opens telebirr. The balance changes when the server has confirmed the payment with telebirr.
  - Until the telebirr merchant agreement is live, the server answers `rail_unavailable`, and the app says top-ups aren't available yet.
- **Idempotency.**
  - Every state-changing call carries an `Idempotency-Key`.
  - A retry of the same request reuses its key; a changed request gets a new one. So a timeout followed by a retry can't create two payments.
- **The review screen comes first.** The recipient's verified name, the fee and the total appear before the PIN is asked for. Leaving the review screen cancels the unconfirmed payment.
- **Failures.** A failure says what went wrong. Where the server says money was not taken (`charged: false`), the app says so too.
- **Tokens.**
  - The refresh token lives in the Android Keystore or the iOS Keychain (`flutter_secure_storage`). The access token is kept only in memory.
  - When several calls get a 401 at once, they share one refresh. Refresh tokens rotate, and reusing an old one revokes the whole session.

## Languages

There are four: Amharic (the default), Afaan Oromo, Tigrinya and English. All copy is in `lib/l10n/strings.dart`.

- **English and Amharic** are complete.
- **Afaan Oromo and Tigrinya are partial first drafts.** Missing keys fall back to English. A native-speaking translator must review and complete both before release.
- **Framework labels.** Flutter ships its own Material labels (back, paste and so on) for Amharic and English but not for Afaan Oromo or Tigrinya, so those labels show in English.
- **Ge'ez script.** Amharic and Tigrinya use the design system's Ethiopic text styles, one step larger with taller lines. Noto Sans Ethiopic is the fallback in every font stack, so mixed-script text renders.
- **Dates.** Dates are numeric for now (`Today, 08:42`, `18/11/2026, 08:42`). The design's `Mon 18 Nov` format and the Ethiopian calendar setting are still to do.

## Build and run

Flutter isn't installed on this machine. `flutterw.sh` runs Flutter inside Docker:
- The SDK lives in the `g1flutter` volume. It was unpacked from the official `flutter_linux_3.24.5-stable.tar.xz`, checked against its published SHA-256.
- Packages are cached in the `g1pub` volume.

```sh
# from app/ (Git Bash)
./flutterw.sh 'flutter pub get && flutter analyze && flutter test'   # expect: No issues found, 15 tests pass
```

To rebuild the SDK volume:
1. Download the tarball from `storage.googleapis.com/flutter_infra_release/releases/stable/linux/`. On a flaky connection, `curl -C -` resumes.
2. Check its SHA-256 against `releases_linux.json`.
3. Unpack it into `g1flutter` at `/opt/flutter`.

Building an APK also needs the Android SDK, which isn't set up yet. Once it is, run:
`flutter build apk --debug --dart-define=API_BASE_URL=http://10.0.2.2:8080`. `10.0.2.2` is the host machine as seen from the Android emulator.

Platform settings:
- **Android:** internet and camera permissions, with the camera marked optional. App backup is off. Plain HTTP is allowed only by the debug manifest, for a local API. There's a `queries` entry so the telebirr checkout link can open.
- **iOS:** the camera prompt text, and local-network HTTP for development.

Production builds must point `API_BASE_URL` at an `https` address.

## Not done yet

- Biometric confirmation. It needs the server's DPoP and biometric-key work; the PIN is the only method for now.
- Push notifications. They need the outbox relay.
- Request money, bills, airtime and orders. Their backend modules don't exist yet.
- PIN reset by SMS code. The server supports it, but the app has no screen for it yet.
- The Ethiopian calendar and localized date formats.
- Review of the Afaan Oromo and Tigrinya copy, as above.
