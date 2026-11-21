import 'dart:async';
import 'dart:io' show Platform;

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/phone.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';

/// Welcome and phone number. Language can be chosen before signing in.
class PhoneScreen extends StatefulWidget {
  const PhoneScreen({super.key});

  @override
  State<PhoneScreen> createState() => _PhoneScreenState();
}

class _PhoneScreenState extends State<PhoneScreen> {
  final _phone = TextEditingController();
  String? _error;
  ApiProblem? _problem;
  bool _busy = false;

  @override
  void dispose() {
    _phone.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    final s = S.of(context);
    final e164 = normalizePhone(_phone.text);
    if (e164 == null) {
      setState(() => _error = s.t('phone_invalid'));
      return;
    }
    setState(() {
      _error = null;
      _problem = null;
      _busy = true;
    });
    final app = AppScope.read(context);
    try {
      final ch = await app.api.requestOtp(e164, 'login', app.locale.languageCode);
      if (!mounted) return;
      Navigator.of(context).push(MaterialPageRoute(
          builder: (_) => OtpScreen(phone: e164, challengeId: ch.challengeId, resendAfter: ch.resendAfter)));
    } on ApiProblem catch (p) {
      setState(() => _problem = p);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final app = AppScope.of(context);
    return Scaffold(
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.fromLTRB(G1.s4, G1.s4, G1.s4, G1.s6),
          children: [
            Align(
              alignment: AlignmentDirectional.centerEnd,
              child: _LanguageButton(current: app.locale, onChanged: app.setLocale),
            ),
            const SizedBox(height: G1.s8),
            Text('Gebeya One', style: text.displayMedium?.copyWith(color: c.brand)),
            const SizedBox(height: G1.s6),
            Text(s.t('welcome_title'), style: text.titleLarge),
            const SizedBox(height: G1.s2),
            Text(s.t('welcome_body'), style: text.bodyMedium?.copyWith(color: c.inkMuted)),
            const SizedBox(height: G1.s8),
            TextField(
              controller: _phone,
              keyboardType: TextInputType.phone,
              autofillHints: const [AutofillHints.telephoneNumber],
              inputFormatters: [FilteringTextInputFormatter.allow(RegExp(r'[0-9+ ]'))],
              style: text.titleMedium?.copyWith(fontFeatures: G1.tabular),
              decoration: InputDecoration(
                labelText: s.t('phone_label'),
                hintText: '09XX XXX XXX',
                prefixIcon: const Icon(Icons.phone_iphone),
                errorText: _error,
              ),
              onSubmitted: (_) => _submit(),
            ),
            if (_problem != null) ...[const SizedBox(height: G1.s4), ProblemBanner(_problem!)],
            const SizedBox(height: G1.s6),
            PrimaryButton(label: s.t('send_code'), busy: _busy, onPressed: _submit),
          ],
        ),
      ),
    );
  }
}

class _LanguageButton extends StatelessWidget {
  const _LanguageButton({required this.current, required this.onChanged});
  final Locale current;
  final ValueChanged<Locale> onChanged;

  @override
  Widget build(BuildContext context) => PopupMenuButton<String>(
        tooltip: S.of(context).t('choose_language'),
        initialValue: current.languageCode,
        onSelected: (code) => onChanged(Locale(code)),
        itemBuilder: (_) => [
          for (final e in S.languageNames.entries) PopupMenuItem(value: e.key, child: Text(e.value)),
        ],
        child: Padding(
          padding: const EdgeInsets.all(G1.s2),
          child: Row(mainAxisSize: MainAxisSize.min, children: [
            const Icon(Icons.language, size: 20),
            const SizedBox(width: G1.s1),
            Text(S.languageNames[current.languageCode] ?? current.languageCode),
          ]),
        ),
      );
}

/// Six-digit SMS code. Verifies as soon as all digits are in.
class OtpScreen extends StatefulWidget {
  const OtpScreen({super.key, required this.phone, required this.challengeId, required this.resendAfter});

  final String phone;
  final String challengeId;
  final int resendAfter;

  @override
  State<OtpScreen> createState() => _OtpScreenState();
}

class _OtpScreenState extends State<OtpScreen> {
  final _code = TextEditingController();
  late String _challenge = widget.challengeId;
  late int _wait = widget.resendAfter;
  Timer? _timer;
  ApiProblem? _problem;
  bool _busy = false;

  @override
  void initState() {
    super.initState();
    _startTimer();
  }

  void _startTimer() {
    _timer?.cancel();
    _timer = Timer.periodic(const Duration(seconds: 1), (t) {
      if (_wait <= 0) {
        t.cancel();
      } else {
        setState(() => _wait--);
      }
    });
  }

  @override
  void dispose() {
    _timer?.cancel();
    _code.dispose();
    super.dispose();
  }

  Future<void> _verify() async {
    if (_code.text.length != 6 || _busy) return;
    setState(() {
      _busy = true;
      _problem = null;
    });
    final app = AppScope.read(context);
    try {
      final needsPin = await app.api.verifyOtp(_challenge, _code.text,
          platform: Platform.isIOS ? 'ios' : 'android', model: Platform.operatingSystemVersion);
      if (!mounted) return;
      Navigator.of(context).popUntil((r) => r.isFirst);
      await app.signedIn(needsPin: needsPin);
    } on ApiProblem catch (p) {
      _code.clear();
      setState(() => _problem = p);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _resend() async {
    final app = AppScope.read(context);
    setState(() => _problem = null);
    try {
      final ch = await app.api.requestOtp(widget.phone, 'login', app.locale.languageCode);
      setState(() {
        _challenge = ch.challengeId;
        _wait = ch.resendAfter;
      });
      _startTimer();
    } on ApiProblem catch (p) {
      setState(() {
        _problem = p;
        if (p.retryAfter != null) _wait = p.retryAfter!.inSeconds;
      });
      _startTimer();
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    return Scaffold(
      appBar: AppBar(),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(G1.s4),
          children: [
            Text(s.t('otp_title'), style: text.titleLarge),
            const SizedBox(height: G1.s2),
            Text(s.t('otp_body', {'phone': displayPhone(widget.phone)}),
                style: text.bodyMedium?.copyWith(color: c.inkMuted)),
            const SizedBox(height: G1.s6),
            TextField(
              controller: _code,
              autofocus: true,
              keyboardType: TextInputType.number,
              autofillHints: const [AutofillHints.oneTimeCode],
              maxLength: 6,
              textAlign: TextAlign.center,
              inputFormatters: [FilteringTextInputFormatter.digitsOnly],
              style: text.displayMedium?.copyWith(letterSpacing: 12, fontFeatures: G1.tabular),
              decoration: const InputDecoration(counterText: '', hintText: '••••••'),
              onChanged: (v) {
                if (v.length == 6) _verify();
              },
            ),
            const SizedBox(height: G1.s4),
            if (_busy) const Center(child: CircularProgressIndicator()),
            if (_problem != null) ProblemBanner(_problem!),
            const SizedBox(height: G1.s4),
            Center(
              child: _wait > 0
                  ? Text(s.t('otp_resend_in', {'s': '$_wait'}), style: text.bodySmall)
                  : TextButton(onPressed: _resend, child: Text(s.t('otp_resend'))),
            ),
          ],
        ),
      ),
    );
  }
}

/// First PIN: enter twice. The server rejects weak PINs (123456, 111111 and the like).
class PinSetupScreen extends StatefulWidget {
  const PinSetupScreen({super.key});

  @override
  State<PinSetupScreen> createState() => _PinSetupScreenState();
}

class _PinSetupScreenState extends State<PinSetupScreen> {
  String? _first;
  String? _error;
  bool _busy = false;

  Future<void> _entered(String pin) async {
    final s = S.of(context);
    if (_first == null) {
      setState(() {
        _first = pin;
        _error = null;
      });
      return;
    }
    if (pin != _first) {
      setState(() {
        _first = null;
        _error = s.t('pin_mismatch');
      });
      return;
    }
    setState(() => _busy = true);
    final app = AppScope.read(context);
    try {
      await app.api.setPin(pin);
      await app.pinSet();
    } on ApiProblem catch (p) {
      setState(() {
        _first = null;
        _error = p.message.isNotEmpty ? p.message : s.t('error_generic');
      });
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    return Scaffold(
      body: SafeArea(
        child: Padding(
          padding: const EdgeInsets.all(G1.s4),
          child: Column(children: [
            const SizedBox(height: G1.s8),
            Text(_first == null ? s.t('pin_create_title') : s.t('pin_repeat_title'),
                style: text.titleLarge, textAlign: TextAlign.center),
            const SizedBox(height: G1.s2),
            if (_first == null)
              Text(s.t('pin_create_body'),
                  textAlign: TextAlign.center, style: text.bodyMedium?.copyWith(color: c.inkMuted)),
            const Spacer(),
            PinPad(key: ValueKey(_first == null), onComplete: _entered, error: _error, busy: _busy),
            const SizedBox(height: G1.s4),
          ]),
        ),
      ),
    );
  }
}
