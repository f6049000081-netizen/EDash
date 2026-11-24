import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:gebeya_one/l10n/strings.dart';

void main() {
  test('every language has every text, so nothing falls back to English', () {
    for (final l in S.supported) {
      expect(S.missingKeys(l.languageCode), isEmpty, reason: l.languageCode);
    }
  });

  test('placeholders are filled in every language', () {
    for (final code in ['am', 'om', 'ti', 'en']) {
      final s = S(Locale(code));
      final text = s.t('team_remove_confirm', {'name': 'Hana', 'shop': 'Abol'});
      expect(text, allOf(contains('Hana'), contains('Abol'), isNot(contains('{'))), reason: code);
    }
  });
}
