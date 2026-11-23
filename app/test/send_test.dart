import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:gebeya_one/screens/pay.dart';

import 'team_test.dart' show json, pump;

void main() {
  testWidgets('too many new numbers shows the privacy message', (tester) async {
    await pump(tester, const SendScreen(), {
      'POST /v1/payment-intents': (_) => json({
            'code': 'rate_limited',
            'title': 'Too many new numbers',
            'detail': 'server text',
          }, 429),
    });
    await tester.enterText(find.byType(TextField).at(0), '0911223344');
    await tester.enterText(find.byType(TextField).at(1), '50');
    await tester.tap(find.text('Review'));
    await tester.pumpAndSettle();
    expect(find.textContaining('protect people'), findsOneWidget);
    expect(find.text('server text'), findsNothing);
  });
}
