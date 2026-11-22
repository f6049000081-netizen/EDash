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
import 'package:gebeya_one/screens/team.dart';
import 'package:gebeya_one/ui/theme.dart';

http.Response json(Object? body, [int status = 200]) => http.Response(body == null ? '' : jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

Future<List<http.Request>> pump(
    WidgetTester tester, Widget home, Map<String, http.Response Function(http.Request)> routes) async {
  tester.view.physicalSize = const Size(1200, 3000);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  final seen = <http.Request>[];
  final state = AppState(
      GebeyaApi(Api(
        baseUrl: 'http://api',
        store: MemoryTokenStore(),
        client: MockClient((r) async {
          seen.add(r);
          final f = routes['${r.method} ${r.url.path}'];
          return f != null ? f(r) : json({'code': 'not_found'}, 404);
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
      home: home,
    ),
  ));
  await tester.pumpAndSettle();
  return seen;
}

Merchant shop(String role) => Merchant.fromJson({
      'id': 'm1',
      'trade_name': 'Selam Café',
      'status': 'active',
      'role': role,
      'category_code': '5812',
      'city': 'Addis Ababa',
    });

Map<String, dynamic> teamJson({List<Map<String, dynamic>> invitations = const []}) => {
      'members': [
        {
          'user_id': 'u1',
          'name': 'Tsion',
          'phone': '••4471',
          'role': 'owner',
          'since': '2026-11-01T08:00:00Z',
          'you': true
        },
        {
          'user_id': 'u2',
          'name': 'Dawit',
          'phone': '••5305',
          'role': 'cashier',
          'since': '2026-11-10T08:00:00Z',
          'you': false
        },
      ],
      'invitations': invitations,
    };

Map<String, dynamic> invitationJson({String? phone}) => {
      'id': 'i1',
      'merchant_id': 'm1',
      'shop': 'Selam Café',
      'role': 'accountant',
      'invited_by': 'Tsion',
      'created_at': '2026-11-22T08:00:00Z',
      'expires_at': '2026-11-29T08:00:00Z',
      if (phone != null) 'phone': phone,
    };

void main() {
  testWidgets('an owner invites by local number, cancels it, and changes a role', (tester) async {
    var invited = false;
    Map<String, dynamic>? inviteBody, roleBody;
    final seen = await pump(tester, TeamScreen(shop('owner')), {
      'GET /v1/merchants/m1/team': (_) =>
          json(teamJson(invitations: [if (invited) invitationJson(phone: '+251911234567')])),
      'POST /v1/merchants/m1/invitations': (r) {
        inviteBody = jsonDecode(r.body) as Map<String, dynamic>;
        invited = true;
        return json(invitationJson(phone: '+251911234567'), 201);
      },
      'DELETE /v1/merchants/m1/invitations/i1': (_) {
        invited = false;
        return json(null, 204);
      },
      'PUT /v1/merchants/m1/members/u2': (r) {
        roleBody = jsonDecode(r.body) as Map<String, dynamic>;
        return json(null, 204);
      },
    });
    expect(find.text('Tsion (you)'), findsOneWidget);
    expect(find.text('Cashier · ••5305'), findsOneWidget);
    expect(find.text('No open invitations.'), findsOneWidget);

    // A bad number is caught before sending.
    await tester.tap(find.text('Invite someone'));
    await tester.pumpAndSettle();
    await tester.enterText(find.byType(TextField), '12');
    await tester.tap(find.text('Send invitation'));
    await tester.pumpAndSettle();
    expect(find.text('Enter a full phone number.'), findsOneWidget);
    expect(inviteBody, isNull);

    await tester.enterText(find.byType(TextField), '0911 23 45 67');
    await tester.tap(find.text('Accountant').last);
    await tester.tap(find.text('Send invitation'));
    await tester.pumpAndSettle();
    expect(inviteBody, {'phone': '+251911234567', 'role': 'accountant'});
    expect(find.text('Invitation sent to +251 91 123 4567.'), findsOneWidget);
    expect(find.text('+251 91 123 4567'), findsOneWidget);

    await tester.tap(find.widgetWithText(TextButton, 'Cancel'));
    await tester.pumpAndSettle();
    expect(seen.any((r) => r.method == 'DELETE'), isTrue);
    expect(find.text('No open invitations.'), findsOneWidget);

    // The owner's own row can't be opened; Dawit's can.
    await tester.tap(find.text('Dawit'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('Manager').last);
    await tester.pumpAndSettle();
    await tester.tap(find.text('Save role'));
    await tester.pumpAndSettle();
    expect(roleBody, {'role': 'manager'});
    expect(find.text('Role changed. They were told.'), findsOneWidget);
  });

  testWidgets('a manager sees the team but cannot change it', (tester) async {
    await pump(tester, TeamScreen(shop('manager')), {
      'GET /v1/merchants/m1/team': (_) => json(teamJson()),
    });
    expect(find.text('Dawit'), findsOneWidget);
    expect(find.text('Invite someone'), findsNothing);
    await tester.tap(find.text('Dawit'));
    await tester.pumpAndSettle();
    expect(find.text('Save role'), findsNothing);
  });

  testWidgets('an invitation shows who sent it and can be declined', (tester) async {
    var list = [invitationJson()];
    final seen = await pump(tester, const InvitationsScreen(), {
      'GET /v1/me/invitations': (_) => json({'items': list}),
      'POST /v1/me/invitations/i1/decline': (_) {
        list = [];
        return json(null, 204);
      },
    });
    expect(find.text('Selam Café'), findsOneWidget);
    expect(find.text('Tsion invited you to join as Accountant.'), findsOneWidget);
    expect(find.text('Answer by 29/11/2026.'), findsOneWidget);
    await tester.tap(find.text('Decline'));
    await tester.pumpAndSettle();
    expect(seen.where((r) => r.url.path.endsWith('/decline')).length, 1);
    expect(find.text('Invitation declined.'), findsOneWidget);
    expect(find.text('No invitations right now.'), findsOneWidget);
  });
}
