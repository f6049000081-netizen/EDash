import 'dart:convert';
import 'dart:math';

import 'package:http/http.dart' as http;

import 'money.dart';

/// An RFC 9457 problem from the API. `code` is the stable machine value; `message` is already
/// written for people. `charged == false` means money did not leave the wallet.
class ApiProblem implements Exception {
  ApiProblem(this.status, this.code, this.title, this.message, {this.charged, this.retryAfter});

  final int status;
  final String code;
  final String title;
  final String message;
  final bool? charged;
  final Duration? retryAfter;

  static ApiProblem network() =>
      ApiProblem(0, 'network', 'No connection', "We couldn't reach Gebeya One. Check your connection and try again.");

  @override
  String toString() => 'ApiProblem($status $code: $message)';
}

/// Tokens issued by /v1/auth/otp/verify and /v1/auth/token/refresh.
class Tokens {
  Tokens(this.access, this.refresh, this.userId, this.expiresAt);

  final String access;
  final String refresh;
  final String userId;
  final DateTime expiresAt;

  factory Tokens.fromJson(Map<String, dynamic> j) => Tokens(
        j['access_token'] as String,
        j['refresh_token'] as String,
        j['user_id'] as String,
        DateTime.now().add(Duration(seconds: (j['expires_in'] as num).toInt())),
      );
}

/// Where tokens are kept between launches. The app uses the platform keystore; tests use memory.
abstract class TokenStore {
  Future<String?> readRefresh();
  Future<void> writeRefresh(String? token);

  /// A random ID made once per install and kept across sign-outs, so the server recognises the device.
  Future<String> installId();
}

class MemoryTokenStore implements TokenStore {
  String? _v;
  final String _install = newIdempotencyKey();
  @override
  Future<String?> readRefresh() async => _v;
  @override
  Future<void> writeRefresh(String? token) async => _v = token;
  @override
  Future<String> installId() async => _install;
}

/// Thin client for the Gebeya One API. It attaches the access token, refreshes it once on 401,
/// adds an Idempotency-Key to every state-changing call, and turns error bodies into [ApiProblem].
class Api {
  Api({required this.baseUrl, required this.store, http.Client? client, this.onSignedOut})
      : _http = client ?? http.Client();

  final String baseUrl;
  final TokenStore store;
  final http.Client _http;

  /// Called when the refresh token is rejected: the session is over and the user must sign in.
  void Function()? onSignedOut;

  Tokens? _tokens;
  Future<Tokens?>? _refreshing;

  bool get hasSession => _tokens != null;

  Future<void> setTokens(Tokens? t) async {
    _tokens = t;
    await store.writeRefresh(t?.refresh);
  }

  /// Restores a session from the stored refresh token. Returns false if there is none or it no longer works.
  Future<bool> restore() async {
    if (await store.readRefresh() == null) return false;
    return await _refresh() != null;
  }

  Future<Tokens?> _refresh() {
    // Concurrent 401s share one refresh: refresh tokens rotate, so a second use would revoke the family.
    return _refreshing ??= () async {
      try {
        final rt = _tokens?.refresh ?? await store.readRefresh();
        if (rt == null) return null;
        final j = await _send('POST', '/v1/auth/token/refresh', body: {'refresh_token': rt}, auth: false);
        final t = Tokens.fromJson(j);
        await setTokens(t);
        return t;
      } on ApiProblem catch (p) {
        if (p.status == 401 || p.status == 403) {
          await setTokens(null);
          onSignedOut?.call();
          return null;
        }
        rethrow;
      } finally {
        _refreshing = null;
      }
    }();
  }

  Future<Map<String, dynamic>> get(String path, {Map<String, String>? query}) => _authed('GET', path, query: query);

  /// State-changing call. Pass the same [idempotencyKey] when retrying the same action.
  Future<Map<String, dynamic>> post(String path, Map<String, dynamic> body, {String? idempotencyKey}) =>
      _authed('POST', path, body: body, idempotencyKey: idempotencyKey ?? newIdempotencyKey());

  Future<Map<String, dynamic>> put(String path, Map<String, dynamic> body) =>
      _authed('PUT', path, body: body, idempotencyKey: newIdempotencyKey());

  Future<Map<String, dynamic>> patch(String path, Map<String, dynamic> body) => _authed('PATCH', path, body: body);

  /// Calls that need no session (sign-in).
  Future<Map<String, dynamic>> postPublic(String path, Map<String, dynamic> body) =>
      _send('POST', path, body: body, auth: false);

  Future<Map<String, dynamic>> _authed(String method, String path,
      {Map<String, dynamic>? body, Map<String, String>? query, String? idempotencyKey}) async {
    if (_tokens == null || _tokens!.expiresAt.isBefore(DateTime.now().add(const Duration(seconds: 30)))) {
      if (await _refresh() == null) {
        throw ApiProblem(401, 'unauthenticated', 'Sign in again', 'Your session has ended.');
      }
    }
    try {
      return await _send(method, path, body: body, query: query, idempotencyKey: idempotencyKey);
    } on ApiProblem catch (p) {
      if (p.status != 401) rethrow;
      if (await _refresh() == null) rethrow;
      return _send(method, path, body: body, query: query, idempotencyKey: idempotencyKey);
    }
  }

  Future<Map<String, dynamic>> _send(String method, String path,
      {Map<String, dynamic>? body, Map<String, String>? query, String? idempotencyKey, bool auth = true}) async {
    final uri = Uri.parse(baseUrl + path).replace(queryParameters: (query == null || query.isEmpty) ? null : query);
    final req = http.Request(method, uri)..headers['Accept'] = 'application/json';
    if (auth && _tokens != null) req.headers['Authorization'] = 'Bearer ${_tokens!.access}';
    if (idempotencyKey != null) req.headers['Idempotency-Key'] = idempotencyKey;
    if (body != null) {
      req.headers['Content-Type'] = 'application/json';
      req.body = jsonEncode(body);
    }
    final http.Response res;
    try {
      res = await http.Response.fromStream(await _http.send(req).timeout(const Duration(seconds: 30)));
    } catch (_) {
      throw ApiProblem.network();
    }
    if (res.statusCode == 204 || res.body.isEmpty) {
      if (res.statusCode >= 400) {
        throw ApiProblem(res.statusCode, 'http_${res.statusCode}', 'Something went wrong', 'Please try again.');
      }
      return const {};
    }
    Map<String, dynamic> j;
    try {
      j = jsonDecode(utf8.decode(res.bodyBytes)) as Map<String, dynamic>;
    } catch (_) {
      throw ApiProblem(res.statusCode, 'bad_response', 'Something went wrong', 'Please try again.');
    }
    if (res.statusCode >= 400) {
      final ra = int.tryParse(res.headers['retry-after'] ?? '');
      throw ApiProblem(
        res.statusCode,
        (j['code'] as String?) ?? 'http_${res.statusCode}',
        (j['title'] as String?) ?? 'Something went wrong',
        (j['message'] as String?) ?? (j['detail'] as String?) ?? '',
        charged: j['charged'] as bool?,
        retryAfter: ra == null ? null : Duration(seconds: ra),
      );
    }
    return j;
  }
}

final _rng = Random.secure();

/// A random version-4 UUID for the Idempotency-Key header.
String newIdempotencyKey() {
  final b = List<int>.generate(16, (_) => _rng.nextInt(256));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  final h = b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();
  return '${h.substring(0, 8)}-${h.substring(8, 12)}-${h.substring(12, 16)}-${h.substring(16, 20)}-${h.substring(20)}';
}

// ---------------------------------------------------------------------------
// Models: only the fields the app shows.
// ---------------------------------------------------------------------------

class Limits {
  Limits(this.maxBalance, this.dailyRemaining, this.monthlyRemaining);
  final Money maxBalance;
  final Money dailyRemaining;
  final Money monthlyRemaining;
}

class Profile {
  Profile({
    required this.id,
    required this.phone,
    required this.displayName,
    required this.locale,
    required this.kycLevel,
    required this.hasPin,
    this.email,
    this.limits,
  });

  final String id;
  final String phone;
  final String displayName;
  final String locale;
  final String kycLevel;
  final bool hasPin;
  final String? email;
  final Limits? limits;

  factory Profile.fromJson(Map<String, dynamic> j) {
    final l = j['limits'] as Map<String, dynamic>?;
    return Profile(
      id: j['id'] as String,
      phone: j['phone'] as String,
      displayName: (j['display_name'] as String?) ?? '',
      locale: (j['locale'] as String?) ?? 'am',
      kycLevel: (j['kyc_level'] as String?) ?? 'L0',
      hasPin: (j['has_pin'] as bool?) ?? false,
      email: j['email'] as String?,
      limits: l == null
          ? null
          : Limits(
              Money.fromJson(l['max_balance'] as Map<String, dynamic>),
              Money.fromJson(l['daily_out_remaining'] as Map<String, dynamic>),
              Money.fromJson(l['monthly_out_remaining'] as Map<String, dynamic>),
            ),
    );
  }
}

class ActivityItem {
  ActivityItem({
    required this.id,
    required this.type,
    required this.title,
    required this.direction,
    required this.amount,
    required this.status,
    required this.occurredAt,
    this.charged,
    this.failureCode,
  });

  final String id;
  final String type; // payment, transfer_in, transfer_out, topup
  final String title;
  final String direction; // in, out
  final Money amount;
  final String status; // pending, completed, failed, refunded
  final DateTime occurredAt;
  final bool? charged;
  final String? failureCode;

  factory ActivityItem.fromJson(Map<String, dynamic> j) => ActivityItem(
        id: j['id'] as String,
        type: j['type'] as String,
        title: (j['title'] as String?) ?? '',
        direction: j['direction'] as String,
        amount: Money.fromJson(j['amount'] as Map<String, dynamic>),
        status: j['status'] as String,
        occurredAt: DateTime.parse(j['occurred_at'] as String).toLocal(),
        charged: j['charged'] as bool?,
        failureCode: j['failure_code'] as String?,
      );
}

class ActivityPage {
  ActivityPage(this.items, this.nextCursor);
  final List<ActivityItem> items;
  final String? nextCursor;
}

class PaymentIntent {
  PaymentIntent({
    required this.id,
    required this.kind,
    required this.status,
    required this.payeeName,
    required this.payeeVerified,
    required this.amount,
    required this.fee,
    required this.total,
    this.failureCode,
  });

  final String id;
  final String kind;
  final String status; // requires_confirmation, processing, held_for_review, succeeded, failed, cancelled
  final String payeeName;
  final bool payeeVerified;
  final Money amount;
  final Money fee;
  final Money total;
  final String? failureCode;

  factory PaymentIntent.fromJson(Map<String, dynamic> j) {
    final payee = (j['payee'] as Map<String, dynamic>?) ?? const {};
    final failure = j['failure'] as Map<String, dynamic>?;
    return PaymentIntent(
      id: j['id'] as String,
      kind: j['kind'] as String,
      status: j['status'] as String,
      payeeName: (payee['display_name'] as String?) ?? '',
      payeeVerified: (payee['verified_name'] as bool?) ?? false,
      amount: Money.fromJson(j['amount'] as Map<String, dynamic>),
      fee: Money.fromJson(j['fee'] as Map<String, dynamic>),
      total: Money.fromJson(j['total'] as Map<String, dynamic>),
      failureCode: failure?['code'] as String?,
    );
  }
}

class QrPayee {
  QrPayee(this.name, this.verified, this.fixedAmount);
  final String name;
  final bool verified;
  final Money? fixedAmount;
}

/// Typed calls on top of [Api]. One method per endpoint the app uses.
class GebeyaApi {
  GebeyaApi(this.api);
  final Api api;

  Future<({String challengeId, int resendAfter})> requestOtp(String phone, String purpose, String locale) async {
    final j = await api.postPublic('/v1/auth/otp', {'phone': phone, 'purpose': purpose, 'locale': locale});
    return (challengeId: j['challenge_id'] as String, resendAfter: (j['resend_after_seconds'] as num?)?.toInt() ?? 30);
  }

  /// Returns whether the user still needs to set a PIN.
  ///
  /// The device's `attestation` is the install ID for now. That is enough for the dev server, which only
  /// uses it to recognise the device. Production needs a Play Integrity (Android) or App Attest (iOS)
  /// token here, and the production verifiers reject an install ID.
  Future<bool> verifyOtp(String challengeId, String code, {required String platform, required String model}) async {
    final j = await api.postPublic('/v1/auth/otp/verify', {
      'challenge_id': challengeId,
      'code': code,
      'device': {'platform': platform, 'model': model, 'attestation': await api.store.installId()},
    });
    await api.setTokens(Tokens.fromJson(j));
    return (j['needs_pin'] as bool?) ?? false;
  }

  Future<void> setPin(String newPin, {String? currentPin}) =>
      api.put('/v1/auth/pin', {'new_pin': newPin, if (currentPin != null) 'current_pin': currentPin});

  Future<void> logout() async {
    try {
      await api.post('/v1/auth/logout', {});
    } finally {
      await api.setTokens(null);
    }
  }

  Future<Profile> me() async => Profile.fromJson(await api.get('/v1/me'));

  Future<Profile> updateMe({String? displayName, String? email, String? locale}) async =>
      Profile.fromJson(await api.patch('/v1/me', {
        if (displayName != null) 'display_name': displayName,
        if (email != null) 'email': email,
        if (locale != null) 'locale': locale,
      }));

  Future<Money> balance() async {
    final j = await api.get('/v1/wallet/balances');
    final list = (j['balances'] as List).cast<Map<String, dynamic>>();
    final etb = list.firstWhere((b) => (b['available'] as Map)['currency'] == 'ETB', orElse: () => list.first);
    return Money.fromJson(etb['available'] as Map<String, dynamic>);
  }

  Future<ActivityPage> activity({String filter = 'all', String? cursor, int limit = 25}) async {
    final j = await api.get('/v1/activity', query: {
      'filter': filter,
      'limit': '$limit',
      if (cursor != null) 'cursor': cursor,
    });
    return ActivityPage(
      (j['items'] as List).map((e) => ActivityItem.fromJson(e as Map<String, dynamic>)).toList(),
      j['next_cursor'] as String?,
    );
  }

  Future<QrPayee> resolveQr(String payload) async {
    final j = await api.post('/v1/qr/resolve', {'payload': payload});
    final fixed = j['fixed_amount'] as Map<String, dynamic>?;
    return QrPayee((j['display_name'] as String?) ?? '', (j['verified'] as bool?) ?? false,
        fixed == null ? null : Money.fromJson(fixed));
  }

  Future<PaymentIntent> sendToPhone(String phone, Money amount, {String? note, required String key}) async =>
      PaymentIntent.fromJson(await api.post(
          '/v1/payment-intents',
          {
            'kind': 'p2p',
            'payee_phone': phone,
            'amount': amount.toJson(),
            if (note != null && note.isNotEmpty) 'note': note,
          },
          idempotencyKey: key));

  Future<PaymentIntent> payQr(String payload, Money? amount, {required String key}) async =>
      PaymentIntent.fromJson(await api.post(
          '/v1/payment-intents',
          {
            'kind': 'qr_merchant',
            'qr_payload': payload,
            if (amount != null) 'amount': amount.toJson(),
          },
          idempotencyKey: key));

  Future<PaymentIntent> confirm(String intentId, String pin, {required String key}) async => PaymentIntent.fromJson(
      await api.post('/v1/payment-intents/$intentId/confirm', {'pin': pin}, idempotencyKey: key));

  Future<void> cancel(String intentId) => api.post('/v1/payment-intents/$intentId/cancel', {});

  Future<PaymentIntent> intent(String id) async => PaymentIntent.fromJson(await api.get('/v1/payment-intents/$id'));

  /// Starts a telebirr top-up. The checkout URL opens telebirr; the wallet is credited only after
  /// telebirr confirms the payment to the server.
  Future<({PaymentIntent intent, String checkoutUrl})> startTopUp(Money amount, {required String key}) async {
    final j = await api.post('/v1/topups', {'amount': amount.toJson(), 'rail': 'telebirr'}, idempotencyKey: key);
    return (
      intent: PaymentIntent.fromJson(j['intent'] as Map<String, dynamic>),
      checkoutUrl: j['checkout_url'] as String
    );
  }
}
