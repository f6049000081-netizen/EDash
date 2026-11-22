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
import 'package:gebeya_one/screens/computer.dart';
import 'package:gebeya_one/ui/theme.dart';

http.Response json(Object body, [int status = 200]) =>
    http.Response(jsonEncode(body), status, headers: {'content-type': 'application/json; charset=utf-8'});

void main() {
  test('codes are cleaned and shown in two groups', () {
    final f = WebCodeFormatter();
    String typed(String v) => f.formatEditUpdate(TextEditingValue.empty, TextEditingValue(text: v)).text;
    expect(typed('abcd'), 'ABCD');
    expect(typed('abcde'), 'ABCD-E');
    expect(typed('ab cd-ef gh'), 'ABCD-EFGH');
    expect(typed('O0I1abcdefghjk'), 'ABCD-EFGH', reason: 'letters a code never has are dropped, and it stops at 8');
    expect(WebCodeFormatter.clean('ABCD-EFGH'), 'ABCDEFGH');
  });

  testWidgets('a code is checked, approved with the PIN, and the computer can be signed out', (tester) async {
    tester.view.physicalSize = const Size(1080, 2400);
    tester.view.devicePixelRatio = 2.5;
    addTearDown(tester.view.reset);
    final seen = <String>[];
    var sessions = <Map<String, dynamic>>[];
    final state = AppState(
        GebeyaApi(Api(
          baseUrl: 'http://api',
          store: MemoryTokenStore(),
          client: MockClient((r) async {
            seen.add('${r.method} ${r.url.path} ${r.body}');
            final login = {'browser': 'Chrome on Windows', 'ip': '192.0.2.7', 'created_at': '2026-11-22T08:00:00Z'};
            switch ('${r.method} ${r.url.path}') {
              case 'GET /v1/me/web-sessions':
                return json({'items': sessions});
              case 'POST /v1/web-logins/lookup':
                final code = (jsonDecode(r.body) as Map)['code'];
                return code == 'ABCDEFGH'
                    ? json(login)
                    : json(
                        {'code': 'code_not_found', 'title': 'Code not found', 'detail': "That code isn't right."}, 404);
              case 'POST /v1/web-logins/approve':
                if ((jsonDecode(r.body) as Map)['pin'] != '482913') {
                  return json({'code': 'invalid_pin', 'title': 'Wrong PIN', 'detail': "That PIN isn't right."}, 403);
                }
                sessions = [
                  {
                    'id': 'w1',
                    'merchant_id': 'm1',
                    'shop_name': 'Merkato Fresh',
                    'browser': 'Chrome on Windows',
                    'ip': '192.0.2.7',
                    'created_at': '2026-11-22T08:00:00Z',
                    'last_seen_at': '2026-11-22T08:00:00Z'
                  }
                ];
                return json(login);
              case 'DELETE /v1/me/web-sessions/w1':
                sessions = [];
                return http.Response('', 204);
            }
            return json({'code': 'not_found'}, 404);
          }),
        )),
        initialLocale: const Locale('en'));
    await state.api.api.setTokens(Tokens('at', 'rt', 'u1', DateTime.now().add(const Duration(minutes: 10))));
    final merchant = Merchant.fromJson({
      'id': 'm1',
      'trade_name': 'Merkato Fresh',
      'status': 'active',
      'role': 'cashier',
      'category_code': '5411',
      'city': 'Addis Ababa',
      'merchant_no': '12345674',
      'payout_schedule': 'daily',
    });
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
        home: ComputerSignInScreen(merchant),
      ),
    ));
    await tester.pumpAndSettle();
    expect(find.text('No computers are signed in.'), findsOneWidget);

    Future<void> pin(String digits) async {
      for (final d in digits.split('')) {
        await tester.tap(find.bySemanticsLabel(d).last);
        await tester.pump();
      }
      await tester.pumpAndSettle();
    }

    await tester.enterText(find.byType(TextField), 'zzzz-zzzz');
    await tester.pump();
    await tester.tap(find.text('Continue'));
    await tester.pumpAndSettle();
    expect(find.text("That code isn't right."), findsOneWidget);

    await tester.enterText(find.byType(TextField), 'abcd efgh');
    await tester.pump();
    expect(tester.widget<TextField>(find.byType(TextField)).controller!.text, 'ABCD-EFGH');
    await tester.tap(find.text('Continue'));
    await tester.pumpAndSettle();
    expect(find.text('Sign in to Merkato Fresh on this computer?'), findsOneWidget);
    expect(find.text('Chrome on Windows'), findsOneWidget);

    await pin('111111');
    expect(find.text("That PIN isn't right."), findsOneWidget);
    await pin('482913');
    expect(find.text('Chrome on Windows is signed in. It signs out after 2 hours without use.'), findsOneWidget);
    expect(seen.where((s) => s.startsWith('POST /v1/web-logins/approve')).last, contains('"merchant_id":"m1"'));
    expect(find.textContaining('Merkato Fresh ·'), findsOneWidget);

    await tester.tap(find.text('Sign out'));
    await tester.pumpAndSettle();
    expect(seen, contains('DELETE /v1/me/web-sessions/w1 '));
    expect(find.text('No computers are signed in.'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
    await tester.pump(const Duration(seconds: 5)); // let the snackbar go
  });
}
