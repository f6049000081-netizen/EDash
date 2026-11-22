import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;

import 'package:gebeya_one/screens/devices.dart';

import 'team_test.dart' show json, pump;

Map<String, dynamic> device(String id, String model, {bool current = false, bool biometric = false}) => {
      'id': id,
      'platform': 'android',
      'model': model,
      'first_seen_at': '2026-11-01T08:00:00Z',
      'last_active_at': '2026-11-22T08:00:00Z',
      'current': current,
      'biometric': biometric,
    };

Future<void> enterPin(WidgetTester tester, String pin) async {
  for (final d in pin.split('')) {
    await tester.tap(find.text(d).last);
    await tester.pump();
  }
  await tester.pumpAndSettle();
}

void main() {
  testWidgets('another phone is signed out with the PIN; a wrong PIN stays on the pad', (tester) async {
    var list = [
      device('d1', 'Samsung SM-A515F', current: true),
      device('d2', 'Tecno Spark 10', biometric: true),
    ];
    final pins = <String>[];
    await pump(tester, const DevicesScreen(), {
      'GET /v1/me/devices': (_) => json({'items': list}),
      'GET /v1/me/web-sessions': (_) => json({'items': []}),
      'POST /v1/me/devices/d2/sign-out': (r) {
        final pin = (jsonDecode(r.body) as Map)['pin'] as String;
        pins.add(pin);
        if (pin != '204816') return json({'code': 'invalid_pin', 'detail': 'That PIN isn\'t right. Try again.'}, 403);
        list = [list.first];
        return http.Response('', 204);
      },
    });
    expect(find.text('Samsung SM-A515F'), findsOneWidget);
    expect(find.textContaining('This phone'), findsOneWidget);
    expect(find.textContaining('fingerprint payments on'), findsOneWidget);
    expect(find.text('Sign out all other phones'), findsNothing); // only one other phone

    await tester.tap(find.text('Sign out'));
    await tester.pumpAndSettle();
    expect(find.text('Sign out Tecno Spark 10?'), findsOneWidget);
    await enterPin(tester, '111111');
    expect(find.text('That PIN isn\'t right. Try again.'), findsOneWidget);
    await enterPin(tester, '204816');
    expect(pins, ['111111', '204816']);
    expect(find.text('Tecno Spark 10 signed out.'), findsOneWidget);
    expect(find.text('Tecno Spark 10'), findsNothing);
  });

  testWidgets('with two other phones, all others sign out at once', (tester) async {
    var list = [device('d1', 'Samsung', current: true), device('d2', 'Tecno'), device('d3', 'Infinix')];
    await pump(tester, const DevicesScreen(), {
      'GET /v1/me/devices': (_) => json({'items': list}),
      'GET /v1/me/web-sessions': (_) => json({
            'items': [
              {
                'id': 'w1',
                'shop_name': 'Selam Café',
                'browser': 'Chrome on Windows',
                'ip': '',
                'last_seen_at': '2026-11-22T08:00:00Z'
              }
            ]
          }),
      'POST /v1/me/devices/sign-out-others': (_) {
        list = [list.first];
        return json({'signed_out': 2});
      },
    });
    expect(find.text('Chrome on Windows'), findsOneWidget);
    await tester.tap(find.text('Sign out all other phones'));
    await tester.pumpAndSettle();
    await enterPin(tester, '204816');
    expect(find.text('Signed out: 2.'), findsOneWidget);
    expect(find.text('Infinix'), findsNothing);
  });
}
