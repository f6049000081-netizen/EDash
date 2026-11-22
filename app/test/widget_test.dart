import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'package:gebeya_one/core/api.dart';
import 'package:gebeya_one/core/app_state.dart';
import 'package:gebeya_one/main.dart';

/// A scripted backend: each test sets the handler for the paths it needs.
AppState appWith(Future<http.Response> Function(http.Request) handler, {Locale locale = const Locale('en')}) =>
    AppState(GebeyaApi(Api(baseUrl: 'http://api', store: MemoryTokenStore(), client: MockClient(handler))),
        initialLocale: locale);

http.Response json(Object body, [int status = 200]) =>
    http.Response(jsonEncode(body), status, headers: {'content-type': 'application/json; charset=utf-8'});

void main() {
  testWidgets('signed out: phone validation, then the code screen', (tester) async {
    final requests = <Map<String, dynamic>>[];
    final state = appWith((r) async {
      if (r.url.path == '/v1/auth/otp') {
        requests.add(jsonDecode(r.body) as Map<String, dynamic>);
        return json({'challenge_id': 'c1', 'expires_at': '2026-11-21T10:00:00Z', 'resend_after_seconds': 30}, 202);
      }
      return json({'code': 'unauthenticated'}, 401);
    });
    await state.start();
    await tester.pumpWidget(EDashApp(state: state));
    await tester.pumpAndSettle();

    await tester.enterText(find.byType(TextField), '0911 23');
    await tester.tap(find.text('Send code'));
    await tester.pump();
    expect(find.text('Enter a full phone number.'), findsOneWidget);
    expect(requests, isEmpty);

    await tester.enterText(find.byType(TextField), '0911 234 567');
    await tester.tap(find.text('Send code'));
    await tester.pumpAndSettle();
    expect(requests.single, {'phone': '+251911234567', 'purpose': 'login', 'locale': 'en'});
    expect(find.text('We sent a 6-digit code to +251 91 123 4567.'), findsOneWidget);
    // Leave the screen so its resend timer stops.
    await tester.pageBack();
    await tester.pumpAndSettle();
  });

  testWidgets('every language renders the sign-in screen', (tester) async {
    for (final code in ['am', 'om', 'ti', 'en']) {
      final state = appWith((r) async => json({'code': 'unauthenticated'}, 401), locale: Locale(code));
      await state.start();
      await tester.pumpWidget(EDashApp(state: state));
      await tester.pumpAndSettle();
      expect(tester.takeException(), isNull, reason: code);
      expect(find.byType(TextField), findsOneWidget, reason: code);
    }
    expect(find.text('ኮድ ላክ'), findsNothing); // last one rendered is English
  });

  testWidgets('home shows balance and recent activity; failed outgoing items are marked', (tester) async {
    var markedRead = false;
    final state = appWith((r) async => switch (r.url.path) {
          '/v1/notifications' => json({
              'items': [
                {
                  'id': 'n1',
                  'kind': 'money_received',
                  'title': 'Money received',
                  'body': 'Abebe sent you ETB 150.00.',
                  'data': {'intent_id': 'i1'},
                  'created_at': DateTime.now().toUtc().toIso8601String(),
                  'read': false,
                },
              ],
              'next_cursor': null,
              'unread': markedRead ? 0 : 1,
            }),
          '/v1/notifications/read' => () {
              markedRead = true;
              return json({'updated': 1});
            }(),
          '/v1/auth/token/refresh' =>
            json({'access_token': 'a', 'refresh_token': 'r2', 'user_id': 'u1', 'expires_in': 900}),
          '/v1/me' => json({
              'id': 'u1',
              'phone': '+251911234567',
              'display_name': 'Hana Bekele',
              'locale': 'en',
              'kyc_level': 'L1',
              'has_pin': true,
              'limits': null,
            }),
          '/v1/wallet/balances' => json({
              'balances': [
                {
                  'available': {'amount_minor': 2458000, 'currency': 'ETB'},
                  'held': {'amount_minor': 0, 'currency': 'ETB'},
                }
              ],
            }),
          '/v1/activity' => json({
              'items': [
                {
                  'id': '1',
                  'type': 'transfer_out',
                  'title': 'Abebe',
                  'direction': 'out',
                  'amount': {'amount_minor': 50000, 'currency': 'ETB'},
                  'status': 'failed',
                  'charged': false,
                  'occurred_at': DateTime.now().toUtc().toIso8601String(),
                },
                {
                  'id': '2',
                  'type': 'topup',
                  'title': 'telebirr',
                  'direction': 'in',
                  'amount': {'amount_minor': 100000, 'currency': 'ETB'},
                  'status': 'completed',
                  'occurred_at': DateTime.now().toUtc().toIso8601String(),
                },
              ],
              'next_cursor': null,
            }),
          _ => json({'code': 'not_found'}, 404),
        });
    await state.api.api.store.writeRefresh('r1');
    await state.start();
    await tester.pumpWidget(EDashApp(state: state));
    await tester.pumpAndSettle();

    expect(find.textContaining('Hana'), findsWidgets);
    expect(find.text('ETB 24,580.00'), findsOneWidget);
    expect(find.text('Failed'), findsOneWidget);
    expect(find.text('+ETB 1,000.00'), findsOneWidget);

    await tester.tap(find.byTooltip('Hide balance'));
    await tester.pump();
    expect(find.text('ETB 24,580.00'), findsNothing);

    // The bell shows the unread count; opening the list marks it read.
    await tester.tap(find.byTooltip('Notifications, 1 unread'));
    await tester.pumpAndSettle();
    expect(find.text('Abebe sent you ETB 150.00.'), findsOneWidget);
    expect(markedRead, isTrue);
    expect(state.unreadNotifications, 0);
  });
}
