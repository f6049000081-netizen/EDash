import 'dart:async';

import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/money.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';
import 'pay.dart';

void openTopUp(BuildContext context) =>
    Navigator.of(context).push(MaterialPageRoute(builder: (_) => const TopUpScreen()));

/// Add money from telebirr. The app never credits anything itself: it opens telebirr's checkout,
/// then waits for the server, which credits the wallet only after telebirr confirms the payment.
/// Until the telebirr merchant agreement is live the server answers rail_unavailable, and this
/// screen says so.
class TopUpScreen extends StatefulWidget {
  const TopUpScreen({super.key});

  @override
  State<TopUpScreen> createState() => _TopUpScreenState();
}

class _TopUpScreenState extends State<TopUpScreen> {
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

  Future<void> _start() async {
    final s = S.of(context);
    final amount = parseAmount(_amount.text);
    if (amount == null || amount.isZero) {
      setState(() => _amountError = s.t('amount_invalid'));
      return;
    }
    if (amount.amountMinor != _keyFor) {
      _key = newIdempotencyKey();
      _keyFor = amount.amountMinor;
    }
    setState(() {
      _amountError = null;
      _problem = null;
      _busy = true;
    });
    final app = AppScope.read(context);
    try {
      final r = await app.api.startTopUp(amount, key: _key);
      final opened = await launchUrl(Uri.parse(r.checkoutUrl), mode: LaunchMode.externalApplication);
      if (!mounted) return;
      Navigator.of(context).pushReplacement(MaterialPageRoute(
          builder: (_) => TopUpWaitingScreen(intent: r.intent, checkoutUrl: r.checkoutUrl, opened: opened)));
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
    final unavailable = _problem?.code == 'rail_unavailable';
    return Scaffold(
      appBar: AppBar(title: Text(s.t('topup_title'))),
      body: SafeArea(
        child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
          Row(children: [
            Container(
              width: 40,
              height: 40,
              decoration: BoxDecoration(color: c.surface2, shape: BoxShape.circle),
              alignment: Alignment.center,
              child: Text('tb', style: text.titleSmall?.copyWith(color: c.inkMuted)),
            ),
            const SizedBox(width: G1.s3),
            Expanded(child: Text(s.t('topup_body'), style: text.bodyMedium)),
          ]),
          const SizedBox(height: G1.s6),
          AmountField(controller: _amount, error: _amountError, autofocus: true),
          if (_problem != null) ...[
            const SizedBox(height: G1.s4),
            unavailable
                ? Container(
                    padding: const EdgeInsets.all(G1.s4),
                    decoration: BoxDecoration(color: c.warningTint, borderRadius: BorderRadius.circular(G1.rM)),
                    child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
                      Icon(Icons.info_outline, color: c.warning),
                      const SizedBox(width: G1.s3),
                      Expanded(child: Text(s.t('topup_unavailable'), style: text.bodyMedium)),
                    ]),
                  )
                : ProblemBanner(_problem!),
          ],
          const SizedBox(height: G1.s6),
          PrimaryButton(label: s.t('topup_continue'), busy: _busy, onPressed: _start),
        ]),
      ),
    );
  }
}

class TopUpWaitingScreen extends StatefulWidget {
  const TopUpWaitingScreen({super.key, required this.intent, required this.checkoutUrl, required this.opened});
  final PaymentIntent intent;
  final String checkoutUrl;
  final bool opened;

  @override
  State<TopUpWaitingScreen> createState() => _TopUpWaitingScreenState();
}

class _TopUpWaitingScreenState extends State<TopUpWaitingScreen> with WidgetsBindingObserver {
  late PaymentIntent _intent = widget.intent;
  Timer? _poll;
  bool _checking = false;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    _poll = Timer.periodic(const Duration(seconds: 5), (_) => _check());
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.resumed) _check(); // back from telebirr
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    _poll?.cancel();
    super.dispose();
  }

  bool get _final => const {'succeeded', 'failed', 'cancelled', 'held_for_review'}.contains(_intent.status);

  Future<void> _check() async {
    if (_checking || _final) return;
    _checking = true;
    final app = AppScope.read(context);
    try {
      final i = await app.api.intent(_intent.id);
      if (!mounted) return;
      setState(() => _intent = i);
      if (_final) {
        _poll?.cancel();
        app.refresh();
      }
    } on ApiProblem {
      // keep waiting; the next tick tries again
    } finally {
      _checking = false;
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    if (_final) {
      return ResultScreen(intent: _intent, successTitle: s.t('topup_done', {'amount': _intent.amount.format()}));
    }
    return Scaffold(
      appBar: AppBar(),
      body: SafeArea(
        child: Padding(
          padding: const EdgeInsets.all(G1.s4),
          child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
            const Spacer(),
            const Center(child: SizedBox(width: 48, height: 48, child: CircularProgressIndicator(strokeWidth: 3))),
            const SizedBox(height: G1.s6),
            Text(s.t('topup_waiting'), textAlign: TextAlign.center, style: text.titleLarge),
            const SizedBox(height: G1.s2),
            Text(s.t('topup_waiting_body'),
                textAlign: TextAlign.center, style: text.bodyMedium?.copyWith(color: c.inkMuted)),
            const SizedBox(height: G1.s4),
            AmountText(_intent.amount, style: text.titleMedium?.copyWith(color: c.inkMuted)),
            const Spacer(),
            if (!widget.opened)
              Padding(
                padding: const EdgeInsets.only(bottom: G1.s2),
                child: PrimaryButton(
                  label: s.t('topup_continue'),
                  onPressed: () => launchUrl(Uri.parse(widget.checkoutUrl), mode: LaunchMode.externalApplication),
                ),
              ),
            OutlinedButton(onPressed: _check, child: Text(s.t('check_status'))),
          ]),
        ),
      ),
    );
  }
}
