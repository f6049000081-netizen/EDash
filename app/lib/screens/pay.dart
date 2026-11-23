import 'dart:convert';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:mobile_scanner/mobile_scanner.dart';

import '../core/api.dart';
import '../core/device_keys.dart';
import '../core/app_state.dart';
import '../core/money.dart';
import '../core/phone.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';
import 'topup.dart';

/// The centre Pay button: send, scan or add money.
Future<void> showPaySheet(BuildContext context) {
  final s = S.of(context);
  return showModalBottomSheet(
    context: context,
    showDragHandle: true,
    shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
    builder: (sheet) => SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s6),
        child: Row(children: [
          Expanded(
              child: QuickAction(
                  icon: Icons.qr_code_scanner,
                  label: s.t('action_scan'),
                  onTap: () {
                    Navigator.pop(sheet);
                    openScan(context);
                  })),
          Expanded(
              child: QuickAction(
                  icon: Icons.north_east,
                  label: s.t('action_send'),
                  onTap: () {
                    Navigator.pop(sheet);
                    openSend(context);
                  })),
          Expanded(
              child: QuickAction(
                  icon: Icons.add,
                  label: s.t('action_add'),
                  onTap: () {
                    Navigator.pop(sheet);
                    openTopUp(context);
                  })),
        ]),
      ),
    ),
  );
}

void openSend(BuildContext context) =>
    Navigator.of(context).push(MaterialPageRoute(builder: (_) => const SendScreen()));

void openScan(BuildContext context) =>
    Navigator.of(context).push(MaterialPageRoute(builder: (_) => const ScanScreen()));

/// Amount field shared by send, QR and top-up. Digits and one decimal point, at most 2 decimals.
class AmountField extends StatelessWidget {
  const AmountField({super.key, required this.controller, this.error, this.autofocus = false, this.onSubmitted});
  final TextEditingController controller;
  final String? error;
  final bool autofocus;
  final ValueChanged<String>? onSubmitted;

  @override
  Widget build(BuildContext context) {
    final text = Theme.of(context).textTheme;
    return TextField(
      controller: controller,
      autofocus: autofocus,
      keyboardType: const TextInputType.numberWithOptions(decimal: true),
      inputFormatters: [FilteringTextInputFormatter.allow(RegExp(r'^\d{0,9}(\.\d{0,2})?'))],
      style: text.displayLarge,
      decoration: InputDecoration(
        labelText: S.of(context).t('amount'),
        prefixText: 'ETB ',
        prefixStyle: text.titleLarge,
        errorText: error,
      ),
      onSubmitted: onSubmitted,
    );
  }
}

class SendScreen extends StatefulWidget {
  const SendScreen({super.key});

  @override
  State<SendScreen> createState() => _SendScreenState();
}

class _SendScreenState extends State<SendScreen> {
  final _phone = TextEditingController();
  final _amount = TextEditingController();
  final _note = TextEditingController();
  String? _phoneError, _amountError;
  ApiProblem? _problem;
  bool _busy = false;

  // One key per distinct request: a retry after a timeout reuses it, an edited request gets a new one.
  String _key = newIdempotencyKey();
  String _keyFor = '';

  @override
  void dispose() {
    _phone.dispose();
    _amount.dispose();
    _note.dispose();
    super.dispose();
  }

  Future<void> _review() async {
    final s = S.of(context);
    final phone = normalizePhone(_phone.text);
    final amount = parseAmount(_amount.text);
    setState(() {
      _phoneError = phone == null ? s.t('phone_invalid') : null;
      _amountError = (amount == null || amount.isZero) ? s.t('amount_invalid') : null;
      _problem = null;
    });
    if (phone == null || amount == null || amount.isZero) return;
    final fingerprint = '$phone|${amount.amountMinor}|${_note.text}';
    if (fingerprint != _keyFor) {
      _key = newIdempotencyKey();
      _keyFor = fingerprint;
    }
    setState(() => _busy = true);
    final app = AppScope.read(context);
    try {
      final intent = await app.api.sendToPhone(phone, amount, note: _note.text.trim(), key: _key);
      if (!mounted) return;
      _keyFor = ''; // the next review is a new payment
      Navigator.of(context).push(MaterialPageRoute(builder: (_) => ReviewScreen(intent: intent)));
    } on ApiProblem catch (p) {
      // Too many new numbers: say it in the app's language.
      setState(() => _problem = p.code == 'rate_limited'
          ? ApiProblem(p.status, p.code, p.title, S.of(context).t('lookups_limited'), retryAfter: p.retryAfter)
          : p);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final app = AppScope.of(context);
    final limits = app.profile?.limits;
    return Scaffold(
      appBar: AppBar(title: Text(s.t('send_title'))),
      body: SafeArea(
        child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
          TextField(
            controller: _phone,
            keyboardType: TextInputType.phone,
            inputFormatters: [FilteringTextInputFormatter.allow(RegExp(r'[0-9+ ]'))],
            decoration: InputDecoration(
              labelText: s.t('recipient_phone'),
              hintText: '09XX XXX XXX',
              prefixIcon: const Icon(Icons.person_outline),
              errorText: _phoneError,
            ),
          ),
          const SizedBox(height: G1.s4),
          AmountField(controller: _amount, error: _amountError),
          if (limits != null) ...[
            const SizedBox(height: G1.s2),
            Text('${s.t('limit_daily')}: ${limits.dailyRemaining.format()}',
                style: Theme.of(context).textTheme.bodySmall),
          ],
          const SizedBox(height: G1.s4),
          TextField(
            controller: _note,
            maxLength: 140,
            decoration: InputDecoration(labelText: s.t('note_optional'), counterText: ''),
          ),
          if (_problem != null) ...[const SizedBox(height: G1.s4), ProblemBanner(_problem!)],
          const SizedBox(height: G1.s6),
          PrimaryButton(label: s.t('review'), busy: _busy, onPressed: _review),
        ]),
      ),
    );
  }
}

class ScanScreen extends StatefulWidget {
  const ScanScreen({super.key});

  @override
  State<ScanScreen> createState() => _ScanScreenState();
}

class _ScanScreenState extends State<ScanScreen> {
  final _scanner = MobileScannerController(formats: const [BarcodeFormat.qrCode]);
  bool _handling = false;
  ApiProblem? _problem;

  @override
  void dispose() {
    _scanner.dispose();
    super.dispose();
  }

  Future<void> _onDetect(BarcodeCapture cap) async {
    final payload = cap.barcodes.firstOrNull?.rawValue;
    if (payload == null || _handling) return;
    setState(() {
      _handling = true;
      _problem = null;
    });
    final app = AppScope.read(context);
    await _scanner.stop();
    try {
      final payee = await app.api.resolveQr(payload);
      if (!mounted) return;
      await Navigator.of(context)
          .pushReplacement(MaterialPageRoute(builder: (_) => QrAmountScreen(payload: payload, payee: payee)));
    } on ApiProblem catch (p) {
      setState(() => _problem = p);
      await _scanner.start();
    } finally {
      if (mounted) setState(() => _handling = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    return Scaffold(
      appBar: AppBar(title: Text(s.t('scan_title'))),
      body: Column(children: [
        Expanded(
          child: Padding(
            padding: const EdgeInsets.all(G1.s4),
            child: ClipRRect(
              borderRadius: BorderRadius.circular(G1.rL),
              child: MobileScanner(
                controller: _scanner,
                onDetect: _onDetect,
                errorBuilder: (context, error, _) => Container(
                  color: c.surface2,
                  padding: const EdgeInsets.all(G1.s6),
                  alignment: Alignment.center,
                  child: Text(s.t('camera_denied'), textAlign: TextAlign.center),
                ),
              ),
            ),
          ),
        ),
        Padding(
          padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s6),
          child: _problem != null
              ? ProblemBanner(_problem!)
              : Text(s.t('scan_hint'), textAlign: TextAlign.center, style: Theme.of(context).textTheme.bodyMedium),
        ),
      ]),
    );
  }
}

/// After a scan: the shop's verified name, then the amount (unless the QR fixes it).
class QrAmountScreen extends StatefulWidget {
  const QrAmountScreen({super.key, required this.payload, required this.payee});
  final String payload;
  final QrPayee payee;

  @override
  State<QrAmountScreen> createState() => _QrAmountScreenState();
}

class _QrAmountScreenState extends State<QrAmountScreen> {
  final _amount = TextEditingController();
  String _key = newIdempotencyKey();
  int? _keyFor;
  String? _amountError;
  ApiProblem? _problem;
  bool _busy = false;

  @override
  void dispose() {
    _amount.dispose();
    super.dispose();
  }

  Future<void> _review() async {
    final s = S.of(context);
    Money? amount;
    if (widget.payee.fixedAmount == null) {
      amount = parseAmount(_amount.text);
      if (amount == null || amount.isZero) {
        setState(() => _amountError = s.t('amount_invalid'));
        return;
      }
    }
    setState(() {
      _amountError = null;
      _problem = null;
      _busy = true;
    });
    final app = AppScope.read(context);
    try {
      // A retry of the same amount reuses the key; a changed amount is a different request.
      if (amount?.amountMinor != _keyFor) {
        _key = newIdempotencyKey();
        _keyFor = amount?.amountMinor;
      }
      final intent = await app.api.payQr(widget.payload, amount, key: _key);
      if (!mounted) return;
      Navigator.of(context).pushReplacement(MaterialPageRoute(builder: (_) => ReviewScreen(intent: intent)));
    } on ApiProblem catch (p) {
      setState(() => _problem = p);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    final fixed = widget.payee.fixedAmount;
    return Scaffold(
      appBar: AppBar(),
      body: SafeArea(
        child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
          _PayeeHeader(name: widget.payee.name, verified: widget.payee.verified, merchant: true),
          const SizedBox(height: G1.s6),
          if (fixed != null)
            AmountText(fixed, style: text.displayLarge)
          else
            AmountField(controller: _amount, error: _amountError, autofocus: true, onSubmitted: (_) => _review()),
          if (_problem != null) ...[const SizedBox(height: G1.s4), ProblemBanner(_problem!)],
          const SizedBox(height: G1.s6),
          PrimaryButton(label: s.t('review'), busy: _busy, onPressed: _review),
        ]),
      ),
    );
  }
}

class _PayeeHeader extends StatelessWidget {
  const _PayeeHeader({required this.name, required this.verified, this.merchant = false});
  final String name;
  final bool verified;
  final bool merchant;

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    return Row(children: [
      Container(
        width: 48,
        height: 48,
        decoration: BoxDecoration(color: c.brandTint, shape: BoxShape.circle),
        child: Icon(merchant ? Icons.storefront_outlined : Icons.person_outline, color: c.brand),
      ),
      const SizedBox(width: G1.s3),
      Expanded(
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Text(name, style: text.titleMedium),
          if (verified)
            Row(children: [
              Icon(Icons.verified, size: 16, color: c.success),
              const SizedBox(width: G1.s1),
              Text(s.t('verified_name'), style: text.bodySmall?.copyWith(color: c.success)),
            ]),
        ]),
      ),
    ]);
  }
}

/// Fees and the recipient's verified name appear before the confirm button, never after.
class ReviewScreen extends StatefulWidget {
  const ReviewScreen({super.key, required this.intent});
  final PaymentIntent intent;

  @override
  State<ReviewScreen> createState() => _ReviewScreenState();
}

class _ReviewScreenState extends State<ReviewScreen> {
  bool _leaving = false;

  Future<void> _cancel() async {
    if (_leaving) return;
    _leaving = true;
    // Best effort: an unconfirmed payment moves no money and expires on its own.
    AppScope.read(context).api.cancel(widget.intent.id).catchError((_) {});
    Navigator.of(context).pop();
  }

  Future<void> _pay() async {
    final app = AppScope.read(context);
    final done = await confirmPayment(context, widget.intent);
    if (done == null || !mounted) return;
    _leaving = true;
    app.refresh();
    Navigator.of(context).pushReplacement(MaterialPageRoute(builder: (_) => ResultScreen(intent: done)));
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    final c = G1.c(context);
    final i = widget.intent;
    Widget line(String label, Money m, {bool strong = false}) => Padding(
          padding: const EdgeInsets.symmetric(vertical: G1.s2),
          child: Row(children: [
            Expanded(
                child: Text(label, style: strong ? text.titleSmall : text.bodyMedium?.copyWith(color: c.inkMuted))),
            AmountText(m, style: strong ? text.titleMedium : text.bodyMedium),
          ]),
        );
    return PopScope(
      canPop: false,
      onPopInvokedWithResult: (didPop, _) {
        if (!didPop) _cancel();
      },
      child: Scaffold(
        appBar: AppBar(title: Text(s.t('review_title'))),
        body: SafeArea(
          child: Padding(
            padding: const EdgeInsets.all(G1.s4),
            child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
              _PayeeHeader(name: i.payeeName, verified: i.payeeVerified, merchant: i.kind == 'qr_merchant'),
              const SizedBox(height: G1.s6),
              Container(
                padding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s2),
                decoration: BoxDecoration(
                    color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
                child: Column(children: [
                  line(s.t('amount'), i.amount),
                  line(s.t('fee'), i.fee),
                  const Divider(),
                  line(s.t('total'), i.total, strong: true),
                ]),
              ),
              const Spacer(),
              PrimaryButton(
                  label: s.t(i.kind == 'p2p' ? 'send_amount' : 'pay_amount', {'amount': i.total.format()}),
                  onPressed: _pay),
              const SizedBox(height: G1.s2),
              TextButton(onPressed: _cancel, child: Text(s.t('cancel'))),
            ]),
          ),
        ),
      ),
    );
  }
}

/// Confirms a payment the way every payment is confirmed: the phone's fingerprint or face check when
/// this phone has it set up, otherwise the PIN. Returns the payment as it ended (a refused one carries
/// the server's reason for [ResultScreen]), or null if the person backed out.
Future<PaymentIntent?> confirmPayment(BuildContext context, PaymentIntent intent) async {
  final result = await _payWithBiometric(context, intent);
  if (result != null || !context.mounted) return result;
  return showModalBottomSheet<PaymentIntent>(
    context: context,
    isScrollControlled: true,
    showDragHandle: true,
    shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
    builder: (_) => _PinSheet(intent: intent),
  );
}

/// Returns null to fall back to the PIN: cancelled, not set up, or refused by the server.
Future<PaymentIntent?> _payWithBiometric(BuildContext context, PaymentIntent intent) async {
  final app = AppScope.read(context);
  final keys = app.api.api.keys;
  final challenge = intent.confirmChallenge;
  if (challenge == null || !await keys.biometricEnrolled()) return null;
  if (!context.mounted) return null;
  final s = S.of(context);
  final String signature;
  try {
    signature = await keys.biometricSign(base64.decode(challenge),
        title: s.t('bio_prompt_title', {'amount': intent.total.format()}),
        subtitle: intent.payeeName,
        cancel: s.t('bio_use_pin'));
  } on BiometricException catch (e) {
    if (e.outcome == BiometricOutcome.notEnrolled) await keys.biometricDelete();
    return null;
  }
  try {
    return await app.api.confirmBiometric(intent.id, signature, key: newIdempotencyKey());
  } on ApiProblem catch (p) {
    if (p.code == 'biometric_not_enrolled') await keys.biometricDelete(); // e.g. turned off by a PIN reset
    if (p.code == 'biometric_not_enrolled' || p.code == 'biometric_failed' || p.code == 'network') return null;
    // Refused by a rule (balance, limit, risk): show the result like the PIN path does.
    PaymentIntent latest;
    try {
      latest = await app.api.intent(intent.id);
    } on ApiProblem {
      latest = intent;
    }
    return _FailedIntent(latest, p);
  }
}

class _PinSheet extends StatefulWidget {
  const _PinSheet({required this.intent});
  final PaymentIntent intent;

  @override
  State<_PinSheet> createState() => _PinSheetState();
}

class _PinSheetState extends State<_PinSheet> {
  String? _error;
  bool _busy = false;

  Future<void> _confirm(String pin) async {
    setState(() {
      _busy = true;
      _error = null;
    });
    final app = AppScope.read(context);
    try {
      final done = await app.api.confirm(widget.intent.id, pin, key: newIdempotencyKey());
      if (mounted) Navigator.of(context).pop(done);
    } on ApiProblem catch (p) {
      if (p.code == 'invalid_pin' || p.code == 'pin_locked' || p.code == 'network') {
        setState(() => _error = p.message.isNotEmpty ? p.message : S.of(context).t('error_generic'));
      } else {
        // Refused by a rule (balance, limit, risk): fetch the final state for the result screen.
        PaymentIntent result;
        try {
          result = await app.api.intent(widget.intent.id);
        } on ApiProblem {
          result = widget.intent;
        }
        if (mounted) Navigator.of(context).pop(_FailedIntent(result, p));
      }
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
        child: Column(mainAxisSize: MainAxisSize.min, children: [
          Text(s.t('pin_enter_title'), style: text.titleLarge),
          const SizedBox(height: G1.s1),
          Text(s.t('pin_enter_body', {'amount': widget.intent.total.format()}), style: text.bodySmall),
          const SizedBox(height: G1.s6),
          PinPad(onComplete: _confirm, error: _error, busy: _busy),
        ]),
      ),
    );
  }
}

/// An intent the server refused at confirmation, with the reason it gave.
class _FailedIntent extends PaymentIntent {
  _FailedIntent(PaymentIntent i, this.problem)
      : super(
          id: i.id,
          kind: i.kind,
          status: 'failed',
          payeeName: i.payeeName,
          payeeVerified: i.payeeVerified,
          amount: i.amount,
          fee: i.fee,
          total: i.total,
          failureCode: problem.code,
        );
  final ApiProblem problem;
}

class ResultScreen extends StatelessWidget {
  const ResultScreen({super.key, required this.intent, this.successTitle});
  final PaymentIntent intent;

  /// Replaces "Sent ETB 500 to Abebe" for results that aren't payments to someone (top-ups).
  final String? successTitle;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final i = intent;
    final (IconData icon, Color fg, Color bg, String title, String body) = switch (i.status) {
      'succeeded' => (
          Icons.check,
          c.success,
          c.successTint,
          successTitle ??
              s.t(i.kind == 'p2p' ? 'sent_to' : 'paid_to', {'amount': i.total.format(), 'name': i.payeeName}),
          '',
        ),
      'held_for_review' || 'processing' => (
          Icons.schedule,
          c.warning,
          c.warningTint,
          s.t('held_title'),
          s.t('held_body')
        ),
      _ => (
          Icons.close,
          c.danger,
          c.dangerTint,
          s.t('failed_title'),
          [
            if (i is _FailedIntent && i.problem.message.isNotEmpty) i.problem.message,
            s.t('not_charged'),
          ].join(' '),
        ),
    };
    return Scaffold(
      body: SafeArea(
        child: Padding(
          padding: const EdgeInsets.all(G1.s4),
          child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
            const Spacer(),
            Center(
              child: TweenAnimationBuilder<double>(
                tween: Tween(begin: MediaQuery.of(context).disableAnimations ? 1 : 0, end: 1),
                duration: const Duration(milliseconds: 400),
                curve: Curves.easeOut,
                builder: (_, v, child) =>
                    Transform.scale(scale: 0.6 + 0.4 * v, child: Opacity(opacity: v, child: child)),
                child: Container(
                  width: 88,
                  height: 88,
                  decoration: BoxDecoration(color: bg, shape: BoxShape.circle),
                  child: Icon(icon, size: 44, color: fg),
                ),
              ),
            ),
            const SizedBox(height: G1.s6),
            Semantics(liveRegion: true, child: Text(title, textAlign: TextAlign.center, style: text.titleLarge)),
            if (body.isNotEmpty) ...[
              const SizedBox(height: G1.s2),
              Text(body, textAlign: TextAlign.center, style: text.bodyMedium?.copyWith(color: c.inkMuted)),
            ],
            const Spacer(),
            PrimaryButton(label: s.t('done'), onPressed: () => Navigator.of(context).pop()),
          ]),
        ),
      ),
    );
  }
}
