import 'dart:async';
import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'package:gebeya_one/core/api.dart';
import 'package:gebeya_one/core/money.dart';
import 'package:gebeya_one/core/phone.dart';

void main() {
  group('Money', () {
    test('formats with grouping and two decimals', () {
      expect(const Money(0, 'ETB').format(), 'ETB 0.00');
      expect(const Money(5, 'ETB').format(), 'ETB 0.05');
      expect(const Money(125000, 'ETB').format(), 'ETB 1,250.00');
      expect(const Money(123456789, 'ETB').format(), 'ETB 1,234,567.89');
      expect(const Money(-2500, 'ETB').format(), 'ETB -25.00');
    });

    test('spoken form names the currency', () {
      expect(const Money(50000, 'ETB').spoken(), '500 Ethiopian birr');
      expect(const Money(50025, 'ETB').spoken(), '500 and 25 cents Ethiopian birr');
    });

    test('parses typed amounts into minor units', () {
      expect(parseAmount('250')?.amountMinor, 25000);
      expect(parseAmount('1,250.5')?.amountMinor, 125050);
      expect(parseAmount(' 0.07 ')?.amountMinor, 7);
      expect(parseAmount('12.')?.amountMinor, 1200);
      for (final bad in ['', '.', 'abc', '1.234', '-5', '1e3', '12.3.4']) {
        expect(parseAmount(bad), isNull, reason: bad);
      }
    });
  });

  group('phone', () {
    test('normalizes Ethiopian numbers', () {
      for (final s in ['0911234567', '911234567', '+251911234567', '251911234567', '00251 91 123 4567', '091-123-4567']) {
        expect(normalizePhone(s), '+251911234567', reason: s);
      }
      expect(normalizePhone('0711234567'), '+251711234567');
    });

    test('keeps other countries as typed with their code', () {
      expect(normalizePhone('+44 7700 900123'), '+447700900123');
      expect(normalizePhone('+1 (202) 555-0143'), '+12025550143');
    });

    test('rejects incomplete or landline-shaped input', () {
      for (final s in ['', '09112', '0111234567', '+251111234567', '12345', '+0123456789', 'phone']) {
        expect(normalizePhone(s), isNull, reason: s);
      }
    });

    test('displays grouped', () {
      expect(displayPhone('+251911234567'), '+251 91 123 4567');
      expect(displayPhone('+447700900123'), '+447700900123');
    });
  });

  group('Api', () {
    Map<String, dynamic> session(String access, String refresh) =>
        {'access_token': access, 'refresh_token': refresh, 'user_id': 'u1', 'expires_in': 900, 'token_type': 'Bearer'};

    test('sends Idempotency-Key on state changes and turns problems into ApiProblem', () async {
      final seen = <http.Request>[];
      final api = Api(
        baseUrl: 'http://api',
        store: MemoryTokenStore(),
        client: MockClient((r) async {
          seen.add(r);
          return http.Response(
              jsonEncode({'code': 'insufficient_funds', 'title': 'Not enough money', 'message': 'Add money first.', 'charged': false}),
              402,
              headers: {'content-type': 'application/problem+json'});
        }),
      );
      await api.setTokens(Tokens.fromJson(session('a1', 'r1')));
      final err = await api.post('/v1/payment-intents', {}).then((_) => null, onError: (e) => e);
      expect(err, isA<ApiProblem>());
      final p = err as ApiProblem;
      expect([p.status, p.code, p.message, p.charged], [402, 'insufficient_funds', 'Add money first.', false]);
      expect(seen.single.headers['Authorization'], 'Bearer a1');
      expect(seen.single.headers['Idempotency-Key'], matches(RegExp(r'^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')));
    });

    test('refreshes once on 401, even for parallel calls, then retries', () async {
      var refreshes = 0;
      final refreshGate = Completer<void>();
      final store = MemoryTokenStore();
      final api = Api(
        baseUrl: 'http://api',
        store: store,
        client: MockClient((r) async {
          if (r.url.path == '/v1/auth/token/refresh') {
            refreshes++;
            expect(jsonDecode(r.body)['refresh_token'], 'r1');
            await refreshGate.future;
            return http.Response(jsonEncode(session('a2', 'r2')), 200);
          }
          if (r.headers['Authorization'] == 'Bearer a2') return http.Response('{"ok":true}', 200);
          return http.Response('{"code":"unauthenticated"}', 401);
        }),
      );
      await api.setTokens(Tokens.fromJson(session('a1', 'r1')));
      final calls = [api.get('/v1/me'), api.get('/v1/wallet/balances'), api.get('/v1/activity')];
      await Future<void>.delayed(const Duration(milliseconds: 10));
      refreshGate.complete();
      final results = await Future.wait(calls);
      expect(results.every((r) => r['ok'] == true), isTrue);
      expect(refreshes, 1, reason: 'a rotated refresh token reused would revoke the whole session');
      expect(await store.readRefresh(), 'r2');
    });

    test('a rejected refresh token signs the user out', () async {
      var signedOut = false;
      final store = MemoryTokenStore();
      final api = Api(
        baseUrl: 'http://api',
        store: store,
        onSignedOut: () => signedOut = true,
        client: MockClient((r) async => http.Response('{"code":"unauthenticated"}', 401)),
      );
      await api.setTokens(Tokens.fromJson(session('a1', 'r1')));
      await expectLater(api.get('/v1/me'), throwsA(isA<ApiProblem>()));
      expect(signedOut, isTrue);
      expect(await store.readRefresh(), isNull);
    });

    test('network failures become a readable problem', () async {
      final api = Api(
          baseUrl: 'http://api', store: MemoryTokenStore(), client: MockClient((r) async => throw Exception('offline')));
      final err = await api.postPublic('/v1/auth/otp', {}).then((_) => null, onError: (e) => e) as ApiProblem;
      expect(err.code, 'network');
    });

    test('parses activity and limits from the API shapes', () async {
      final api = GebeyaApi(Api(
        baseUrl: 'http://api',
        store: MemoryTokenStore(),
        client: MockClient((r) async => switch (r.url.path) {
              '/v1/activity' => http.Response(
                  jsonEncode({
                    'items': [
                      {
                        'id': 'i1',
                        'type': 'transfer_out',
                        'title': 'Abebe',
                        'subtitle': 'Sent',
                        'direction': 'out',
                        'amount': {'amount_minor': 50000, 'currency': 'ETB'},
                        'status': 'failed',
                        'charged': false,
                        'failure_code': 'limit_exceeded',
                        'occurred_at': '2026-11-21T06:00:00Z',
                      }
                    ],
                    'next_cursor': 'abc',
                  }),
                  200),
              _ => http.Response(
                  jsonEncode({
                    'id': 'u1',
                    'phone': '+251911234567',
                    'email': null,
                    'display_name': 'Hana',
                    'locale': 'om',
                    'kyc_level': 'L1',
                    'has_pin': true,
                    'limits': {
                      'max_balance': {'amount_minor': 200000, 'currency': 'ETB'},
                      'daily_out_remaining': {'amount_minor': 0, 'currency': 'ETB'},
                      'monthly_out_remaining': {'amount_minor': 100, 'currency': 'ETB'},
                    },
                  }),
                  200),
            }),
      ));
      await api.api.setTokens(Tokens.fromJson(session('a', 'r')));
      final page = await api.activity(cursor: 'x');
      expect(page.nextCursor, 'abc');
      expect(page.items.single.charged, isFalse);
      expect(page.items.single.amount, const Money(50000, 'ETB'));
      final me = await api.me();
      expect(me.locale, 'om');
      expect(me.limits!.monthlyRemaining.amountMinor, 100);
    });
  });
}
