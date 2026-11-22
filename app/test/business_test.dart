import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'package:gebeya_one/core/api.dart';
import 'package:gebeya_one/core/app_state.dart';
import 'package:gebeya_one/core/business.dart';
import 'package:gebeya_one/l10n/strings.dart';
import 'package:gebeya_one/main.dart';
import 'package:gebeya_one/screens/business.dart';
import 'package:gebeya_one/ui/theme.dart';

http.Response json(Object body, [int status = 200]) =>
    http.Response(jsonEncode(body), status, headers: {'content-type': 'application/json; charset=utf-8'});

Map<String, dynamic> etb(int minor) => {'amount_minor': minor, 'currency': 'ETB'};

Map<String, dynamic> shop(String role, {String status = 'active'}) => {
      'id': 'm1',
      'trade_name': 'Buna Test Café',
      'status': status,
      'role': role,
      'category_code': '5812',
      'city': 'Addis Ababa',
      'merchant_no': status == 'active' ? '12345674' : null,
      'payout_schedule': 'daily',
    };

Future<List<String>> pumpShop(WidgetTester tester, Map<String, dynamic> merchant, {bool cashier = false}) async {
  final seen = <String>[];
  final state = AppState(
      GebeyaApi(Api(
        baseUrl: 'http://api',
        store: MemoryTokenStore(),
        client: MockClient((r) async {
          seen.add(r.url.path);
          return switch (r.url.path) {
            '/v1/merchants/m1' => json(merchant),
            '/v1/merchants/m1/summary' => json({
                'sales_today': etb(136500),
                'payments_today': 2,
                'average_basket': etb(68250),
                'pending_payout': cashier ? null : etb(9950),
                'next_payout': cashier
                    ? null
                    : {
                        'id': 'p1',
                        'net': etb(9950),
                        'status': 'on_hold',
                        'hold_reason': 'no_destination',
                        'scheduled_for': '2026-11-22T04:52:10Z'
                      },
              }),
            '/v1/merchants/m1/payments' => json({
                'items': [
                  {'id': 'i1', 'payer_name': 'Abebe K.', 'amount': etb(22500), 'completed_at': '2026-11-22T08:00:00Z'},
                ],
                'next_cursor': null,
              }),
            '/v1/merchants/m1/payout-destination' => json({'code': 'not_found', 'title': 'Not found'}, 404),
            _ => json({'code': 'not_found'}, 404),
          };
        }),
      )),
      initialLocale: const Locale('en'));
  await state.api.api.setTokens(Tokens('at', 'rt', 'u1', DateTime.now().add(const Duration(minutes: 10))));
  await tester.pumpWidget(AppScope(
    state: state,
    child: MaterialApp(
      theme: g1Theme(Brightness.light, ethiopic: false),
      locale: const Locale('en'),
      supportedLocales: S.supported,
      localizationsDelegates: const [
        SDelegate(),
        FallbackMaterialLocalizations(),
        FallbackCupertinoLocalizations(),
        GlobalMaterialLocalizations.delegate,
        GlobalCupertinoLocalizations.delegate,
        GlobalWidgetsLocalizations.delegate,
      ],
      home: const BusinessScreen('m1'),
    ),
  ));
  await tester.pumpAndSettle();
  return seen;
}

void main() {
  test('merchant numbers print in two groups', () {
    expect(formatMerchantNo('12345674'), '1234 5674');
  });

  testWidgets('an owner sees sales, the payout waiting for an account, and payments', (tester) async {
    final seen = await pumpShop(tester, shop('owner'));
    expect(find.text('ETB 1,365.00'), findsOneWidget);
    expect(find.text('Show my QR'), findsOneWidget);
    expect(find.text('Waiting for a payout account'), findsOneWidget);
    expect(find.text('Not set. Add one to receive your money.'), findsOneWidget);
    expect(find.text('Abebe K.'), findsOneWidget);
    expect(find.text('+ETB 225.00'), findsOneWidget);
    expect(seen, contains('/v1/merchants/m1/payout-destination'));
    await tester.pumpWidget(const SizedBox()); // stops the live-payments timer
  });

  testWidgets("a cashier sees sales and payments but not the shop's payouts", (tester) async {
    final seen = await pumpShop(tester, shop('cashier'), cashier: true);
    expect(find.text('ETB 1,365.00'), findsOneWidget);
    expect(find.text('Abebe K.'), findsOneWidget);
    expect(find.text('Next payout'), findsNothing);
    expect(seen, isNot(contains('/v1/merchants/m1/payout-destination')));
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('an application under review explains the wait and offers no QR', (tester) async {
    final seen = await pumpShop(tester, shop('owner', status: 'kyb_review'));
    expect(find.text("We're reviewing Buna Test Café"), findsOneWidget);
    expect(find.text('Show my QR'), findsNothing);
    expect(seen, isNot(contains('/v1/merchants/m1/summary')));
    await tester.pumpWidget(const SizedBox());
  });
}
