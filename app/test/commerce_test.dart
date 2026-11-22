import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'package:gebeya_one/core/api.dart';
import 'package:gebeya_one/core/app_state.dart';
import 'package:gebeya_one/core/business.dart';
import 'package:gebeya_one/core/commerce.dart';
import 'package:gebeya_one/l10n/strings.dart';
import 'package:gebeya_one/main.dart';
import 'package:gebeya_one/screens/shop.dart';
import 'package:gebeya_one/screens/shop_admin.dart';
import 'package:gebeya_one/ui/theme.dart';

http.Response json(Object body, [int status = 200]) =>
    http.Response(jsonEncode(body), status, headers: {'content-type': 'application/json; charset=utf-8'});

Map<String, dynamic> etb(int minor) => {'amount_minor': minor, 'currency': 'ETB'};

Map<String, dynamic> product(String id, String title, int price, {int? stock, String category = 'Spices'}) => {
      'id': id,
      'title': title,
      'description': '',
      'category': category,
      'price': etb(price),
      'stock': stock,
      'sold_out': stock == 0,
      'status': 'active',
    };

Map<String, dynamic> shopJson({bool open = true}) => {
      'merchant_id': 'm1',
      'name': 'Abebe Spices',
      'category': '5411',
      'city': 'Addis Ababa',
      'open': open,
      'pickup': true,
      'delivery': true,
      'delivery_fee': etb(5000),
      'delivery_area': 'Bole',
      'about': '',
      'product_count': 3,
      'products': [
        product('p1', 'Berbere 500 g', 18000, stock: 2),
        product('p2', 'Mitmita 250 g', 12000),
        product('p3', 'Teff 5 kg', 95000, stock: 0, category: 'Grain'),
      ],
    };

Map<String, dynamic> orderJson(String status,
        {String channel = 'pickup',
        String? code,
        String reason = '',
        String note = '',
        bool forShop = false,
        bool overdue = false,
        bool inReview = false}) =>
    {
      'id': 'o1',
      'number': 'OR-100001',
      'status': status,
      'channel': channel,
      'merchant_id': 'm1',
      'shop_name': 'Abebe Spices',
      'items': [
        {'title': 'Berbere 500 g', 'unit_price': etb(18000), 'quantity': 2, 'line_total': etb(36000)},
      ],
      'subtotal': etb(36000),
      'delivery_fee': etb(channel == 'delivery' ? 5000 : 0),
      'total': etb(channel == 'delivery' ? 41000 : 36000),
      'delivery_address': channel == 'delivery' ? 'Bole, behind Edna Mall' : '',
      'note': '',
      'created_at': '2026-11-22T08:00:00Z',
      'placed_at': status == 'pending_payment' ? null : '2026-11-22T08:00:05Z',
      'accept_by': '2026-11-22T08:20:05Z',
      'payment_intent_id': 'i1',
      'payment_status': status == 'pending_payment' ? 'requires_confirmation' : 'succeeded',
      if (code != null) 'handover_code': code,
      'cancel_reason': reason,
      'cancel_note': note,
      if (forShop) 'customer': {'name': 'Hana T.', 'phone': '••4471'},
      'overdue': overdue || inReview,
      'in_review': inReview,
    };

Future<List<http.Request>> pump(
    WidgetTester tester, Widget home, Map<String, http.Response Function(http.Request)> routes) async {
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

Merchant merchant(String role) => Merchant.fromJson({
      'id': 'm1',
      'trade_name': 'Abebe Spices',
      'status': 'active',
      'role': role,
      'category_code': '5411',
      'city': 'Addis Ababa',
      'merchant_no': '12345674',
      'payout_schedule': 'daily',
    });

void main() {
  group('basket', () {
    final shop = Shop.fromJson(shopJson());
    final berbere = shop.products[0], mitmita = shop.products[1];

    test('keeps quantities within the stock and adds up in minor units', () {
      final b = Basket(shop);
      b.add(berbere);
      b.add(berbere);
      expect(b.add(berbere), 2, reason: 'only two in stock');
      b.set(mitmita, 3);
      expect(b.count, 5);
      expect(b.subtotal.amountMinor, 2 * 18000 + 3 * 12000);
      expect(b.total('pickup').amountMinor, 72000);
      expect(b.total('delivery').amountMinor, 77000);
      expect(b.toJson(), [
        {'product_id': 'p1', 'quantity': 2},
        {'product_id': 'p2', 'quantity': 3},
      ]);
    });

    test('removing the last one takes the line out', () {
      final b = Basket(shop)..add(mitmita);
      b.remove(mitmita);
      expect(b.isEmpty, isTrue);
      expect(b.set(mitmita, 500), 99, reason: 'uncounted products still stop at 99');
    });

    test('groups products by shelf', () {
      expect(shop.shelves.keys, ['Spices', 'Grain']);
    });
  });

  group('order', () {
    test('steps differ for pickup and delivery', () {
      final pickup = Order.fromJson(orderJson('ready'));
      expect(pickup.steps, ['placed', 'accepted', 'ready', 'delivered']);
      expect(pickup.stepsDone, 3);
      expect(pickup.customerCanCancel, isFalse);
      expect(pickup.customerCanConfirm, isTrue);
      final delivery = Order.fromJson(orderJson('on_the_way', channel: 'delivery'));
      expect(delivery.stepsDone, 4);
      expect(delivery.steps.length, 5);
      expect(Order.fromJson(orderJson('placed')).customerCanCancel, isTrue);
      expect(Order.fromJson(orderJson('cancelled')).stepsDone, 0);
    });
  });

  testWidgets('a customer fills the basket within stock and sees what sold out', (tester) async {
    await pump(tester, const ShopScreen('m1'), {'GET /v1/shops/m1': (_) => json(shopJson())});
    expect(find.text('Berbere 500 g'), findsOneWidget);
    expect(find.text('Sold out'), findsOneWidget);
    expect(find.textContaining('Delivers to Bole'), findsOneWidget);
    await tester.tap(find.text('Add').first);
    await tester.pumpAndSettle();
    await tester.tap(find.bySemanticsLabel('One more Berbere 500 g'));
    await tester.pumpAndSettle();
    expect(find.text('Basket · 2 · ETB 360.00'), findsOneWidget);
    final more = tester.widget<IconButton>(
        find.descendant(of: find.bySemanticsLabel('One more Berbere 500 g'), matching: find.byType(IconButton)));
    expect(more.onPressed, isNull, reason: 'only two in stock');
  });

  testWidgets('a closed shop shows its products but takes no orders', (tester) async {
    await pump(tester, const ShopScreen('m1'), {'GET /v1/shops/m1': (_) => json(shopJson(open: false))});
    expect(find.text('Not taking orders right now'), findsOneWidget);
    final add = tester.widget<OutlinedButton>(find.widgetWithText(OutlinedButton, 'Add').first);
    expect(add.onPressed, isNull);
  });

  testWidgets('the customer sees the handover code and confirms they have the order', (tester) async {
    var status = 'ready';
    final seen = await pump(tester, const OrderScreen('o1'), {
      'GET /v1/orders/o1': (_) => json(orderJson(status, code: status == 'ready' ? '4821' : null)),
      'POST /v1/orders/o1/received': (_) {
        status = 'delivered';
        return json(orderJson('delivered'));
      },
    });
    expect(find.text('4821'), findsOneWidget);
    expect(find.text('Ready to collect. Show your handover code at the shop.'), findsOneWidget);
    expect(find.text('Cancel order'), findsNothing, reason: 'the shop has started on it');
    await tester.tap(find.text('I have my order'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('Yes, I have it'));
    await tester.pumpAndSettle();
    expect(seen.map((r) => '${r.method} ${r.url.path}'), contains('POST /v1/orders/o1/received'));
    expect(find.text('Collected. The shop has been paid.'), findsOneWidget);
    expect(find.text('4821'), findsNothing);
    await tester.pumpWidget(const SizedBox()); // stops the poll timer
  });

  testWidgets('an order that was never handed over says so, then shows the refund', (tester) async {
    tester.view.physicalSize = const Size(1080, 2400);
    tester.view.devicePixelRatio = 2.5;
    addTearDown(tester.view.reset);
    var body = orderJson('ready', code: '4821', overdue: true);
    await pump(tester, const OrderScreen('o1'), {'GET /v1/orders/o1': (_) => json(body)});
    expect(find.textContaining('This is taking a while.'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());

    body = orderJson('ready', code: '4821', inReview: true);
    await pump(tester, const OrderScreen('o1'), {'GET /v1/orders/o1': (_) => json(body)});
    expect(find.textContaining('our team is checking with Abebe Spices'), findsOneWidget);
    expect(find.text('I have my order'), findsOneWidget, reason: 'the customer can still confirm');
    await tester.pumpWidget(const SizedBox());

    body = orderJson('cancelled', reason: 'not_completed');
    await pump(tester, const OrderScreen('o1'), {'GET /v1/orders/o1': (_) => json(body)});
    expect(find.text("It wasn't handed over, so our team refunded it."), findsOneWidget);
    expect(find.textContaining('our team is checking'), findsNothing);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets("a declined order shows the shop's note and the refund", (tester) async {
    await pump(tester, const OrderScreen('o1'), {
      'GET /v1/orders/o1': (_) =>
          json(orderJson('cancelled', reason: 'shop_declined', note: 'Sorry, no berbere today')),
    });
    expect(find.text("Abebe Spices couldn't take this order. Your money is back in your wallet."), findsOneWidget);
    expect(find.text('The shop said: "Sorry, no berbere today"'), findsOneWidget);
    expect(find.text('I have my order'), findsNothing);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('the shop accepts an order, then hands it over with the code', (tester) async {
    var status = 'placed';
    final seen = await pump(tester, ShopOrdersScreen(merchant('cashier')), {
      'GET /v1/merchants/m1/orders': (_) => json({
            'items': [orderJson(status, forShop: true)]
          }),
      'POST /v1/merchants/m1/orders/o1/accept': (_) {
        status = 'accepted';
        return json(orderJson(status, forShop: true));
      },
      'POST /v1/merchants/m1/orders/o1/hand_over': (r) {
        if ((jsonDecode(r.body) as Map)['code'] != '4821') {
          return json({'code': 'wrong_code', 'title': 'Wrong code', 'detail': "That code doesn't match."}, 422);
        }
        status = 'delivered';
        return json(orderJson(status, forShop: true));
      },
    });
    expect(find.text('Hana T. · ••4471'), findsOneWidget);
    expect(find.text('Hand over'), findsNothing, reason: 'accept first');
    await tester.tap(find.text('Accept'));
    await tester.pumpAndSettle();
    expect(find.text('Ready to collect'), findsOneWidget);
    await tester.tap(find.text('Hand over'));
    await tester.pumpAndSettle();
    await tester.enterText(find.byType(TextField), '4821');
    await tester.pumpAndSettle();
    await tester.tap(find.widgetWithText(FilledButton, 'Hand over').last);
    await tester.pumpAndSettle();
    final posted = seen.where((r) => r.url.path.endsWith('/hand_over')).single;
    expect(jsonDecode(posted.body), {'code': '4821'});
    expect(find.text('OR-100001 handed over. ETB 360.00 is on its way to your payouts.'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('an owner adds a product photo from the catalog, then removes it', (tester) async {
    tester.view.physicalSize = const Size(1080, 2400);
    tester.view.devicePixelRatio = 2.5;
    addTearDown(tester.view.reset);
    var withPhoto = false;
    Map<String, dynamic> berbere() => {
          ...product('p1', 'Berbere 500 g', 18000, stock: 2),
          'photo': withPhoto
              ? {
                  'id': 'a1b2c3d4e5f6a1b2c3d4e5f6',
                  'url': '/v1/products/p1/photos/a1b2c3d4e5f6a1b2c3d4e5f6',
                  'thumb_url': '/v1/products/p1/photos/a1b2c3d4e5f6a1b2c3d4e5f6?size=thumb'
                }
              : null,
        };
    final picked = <int>[0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3];
    final seen = await pump(tester, CatalogScreen(merchant('owner'), pick: (_) async => picked), {
      'GET /v1/merchants/m1/shop': (_) => json({
            ...shopJson(),
            'products': [berbere()]
          }),
      'PUT /v1/merchants/m1/products/p1/photo': (r) {
        withPhoto = true;
        return json(berbere());
      },
      'DELETE /v1/merchants/m1/products/p1/photo': (_) {
        withPhoto = false;
        return json(berbere());
      },
    });
    await tester.tap(find.text('Berbere 500 g'));
    await tester.pumpAndSettle();
    expect(find.text('Remove photo'), findsNothing);
    await tester.tap(find.text('Add photo'));
    await tester.pumpAndSettle();
    final put = seen.lastWhere((r) => r.method == 'PUT');
    expect(put.url.path, '/v1/merchants/m1/products/p1/photo');
    expect(put.bodyBytes, picked, reason: 'the picked photo is sent as the body');
    expect(put.headers['content-type'], startsWith('image/jpeg'));
    expect(find.text('Change photo'), findsOneWidget);
    await tester.tap(find.text('Remove photo'));
    await tester.pumpAndSettle();
    expect(seen.map((r) => '${r.method} ${r.url.path}'), contains('DELETE /v1/merchants/m1/products/p1/photo'));
    expect(find.text('Add photo'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('an accountant sees orders but cannot act on them', (tester) async {
    await pump(tester, ShopOrdersScreen(merchant('accountant')), {
      'GET /v1/merchants/m1/orders': (_) => json({
            'items': [orderJson('placed', forShop: true)]
          }),
    });
    expect(find.text('OR-100001 · Pickup'), findsOneWidget);
    expect(find.text('Accept'), findsNothing);
    await tester.pumpWidget(const SizedBox());
  });
}
