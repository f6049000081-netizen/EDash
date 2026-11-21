/// Turns what a person typed into an E.164 number, or null if it can't be one.
///
/// Ethiopian numbers may be typed as 0911 234 567, 911234567 or +251 911 234 567. Mobile
/// numbers start with 9 (Ethio telecom) or 7 (Safaricom Ethiopia). Numbers from other countries
/// must start with + and their country code; the server decides which countries are allowed.
String? normalizePhone(String input) {
  final raw = input.replaceAll(RegExp(r'[\s\-().]'), '');
  if (raw.startsWith('+')) {
    final digits = raw.substring(1);
    if (!RegExp(r'^[1-9]\d{7,14}$').hasMatch(digits)) return null;
    if (digits.startsWith('251')) return _ethiopian(digits.substring(3));
    return '+$digits';
  }
  if (raw.startsWith('00')) return normalizePhone('+${raw.substring(2)}');
  if (raw.startsWith('251') && raw.length == 12) return _ethiopian(raw.substring(3));
  if (raw.startsWith('0')) return _ethiopian(raw.substring(1));
  return _ethiopian(raw);
}

String? _ethiopian(String national) =>
    RegExp(r'^[79]\d{8}$').hasMatch(national) ? '+251$national' : null;

/// "+251 91 123 4567" for display.
String displayPhone(String e164) {
  final m = RegExp(r'^\+251(\d{2})(\d{3})(\d{4})$').firstMatch(e164);
  if (m == null) return e164;
  return '+251 ${m.group(1)} ${m.group(2)} ${m.group(3)}';
}
