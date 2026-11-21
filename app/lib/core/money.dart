/// Money is integer minor units plus an ISO 4217 code, exactly as the API sends it.
/// 1 ETB = 100 santim. Doubles never touch an amount.
class Money {
  const Money(this.amountMinor, this.currency);

  final int amountMinor;
  final String currency;

  factory Money.fromJson(Map<String, dynamic> j) =>
      Money((j['amount_minor'] as num).toInt(), j['currency'] as String);

  Map<String, dynamic> toJson() => {'amount_minor': amountMinor, 'currency': currency};

  static const zeroEtb = Money(0, 'ETB');

  bool get isZero => amountMinor == 0;

  @override
  bool operator ==(Object other) =>
      other is Money && other.amountMinor == amountMinor && other.currency == currency;

  @override
  int get hashCode => Object.hash(amountMinor, currency);

  /// "1,250.00": grouped whole part and two decimals, no currency.
  String get decimal {
    final neg = amountMinor < 0;
    final a = amountMinor.abs();
    final whole = (a ~/ 100).toString();
    final frac = (a % 100).toString().padLeft(2, '0');
    final buf = StringBuffer();
    for (var i = 0; i < whole.length; i++) {
      if (i > 0 && (whole.length - i) % 3 == 0) buf.write(',');
      buf.write(whole[i]);
    }
    return '${neg ? '-' : ''}$buf.$frac';
  }

  /// "ETB 1,250.00". The brand book: always show the ISO code with the amount.
  String format() => '$currency $decimal';

  /// Spoken form for screen readers: "1250 point 00 Ethiopian birr" reads badly, so say the
  /// whole and minor parts with the currency's name.
  String spoken() {
    final a = amountMinor.abs();
    final names = {'ETB': 'Ethiopian birr', 'USD': 'US dollars', 'EUR': 'euros', 'GBP': 'pounds sterling'};
    final name = names[currency] ?? currency;
    final cents = a % 100;
    return '${amountMinor < 0 ? 'minus ' : ''}${a ~/ 100}${cents > 0 ? ' and $cents cents' : ''} $name';
  }

  @override
  String toString() => format();
}

/// Parses what a person typed ("250", "1,250.5", "1250.50") into minor units.
/// Returns null for anything else: more than two decimals, letters, signs, empty input.
Money? parseAmount(String input, {String currency = 'ETB'}) {
  final s = input.replaceAll(',', '').replaceAll(' ', '').trim();
  final m = RegExp(r'^(\d{1,12})(?:\.(\d{0,2}))?$').firstMatch(s);
  if (m == null) return null;
  final whole = int.parse(m.group(1)!);
  final fracStr = (m.group(2) ?? '').padRight(2, '0');
  final minor = whole * 100 + int.parse(fracStr);
  return Money(minor, currency);
}
