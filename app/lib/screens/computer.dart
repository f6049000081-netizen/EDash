import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/business.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';

/// Upper-cases what's typed and shows it as ABCD-EFGH, leaving out characters a code never has.
class WebCodeFormatter extends TextInputFormatter {
  static const _alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';

  static String clean(String v) => v.toUpperCase().split('').where(_alphabet.contains).take(8).join();

  @override
  TextEditingValue formatEditUpdate(TextEditingValue oldValue, TextEditingValue newValue) {
    final c = clean(newValue.text);
    final shown = c.length > 4 ? '${c.substring(0, 4)}-${c.substring(4)}' : c;
    return TextEditingValue(text: shown, selection: TextSelection.collapsed(offset: shown.length));
  }
}

/// Signing in to the business console on a computer: the computer shows a code, the person types it here,
/// checks which browser is asking, and approves it with their PIN. Also lists the browsers signed in.
class ComputerSignInScreen extends StatefulWidget {
  const ComputerSignInScreen(this.merchant, {super.key});
  final Merchant merchant;

  @override
  State<ComputerSignInScreen> createState() => _ComputerSignInScreenState();
}

class _ComputerSignInScreenState extends State<ComputerSignInScreen> {
  final _code = TextEditingController();
  String? _error;
  bool _busy = false;
  List<WebSession>? _sessions;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _loadSessions());
  }

  @override
  void dispose() {
    _code.dispose();
    super.dispose();
  }

  String _problemText(ApiProblem p) => p.message.isNotEmpty ? p.message : S.of(context).t('error_generic');

  Future<void> _loadSessions() async {
    try {
      final list = await AppScope.read(context).api.webSessions();
      if (mounted) setState(() => _sessions = list);
    } on ApiProblem {
      if (mounted) setState(() => _sessions = const []);
    }
  }

  Future<void> _continue() async {
    final code = WebCodeFormatter.clean(_code.text);
    final api = AppScope.read(context).api;
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final login = await api.lookupWebLogin(code);
      if (!mounted) return;
      setState(() => _busy = false);
      final approved = await showModalBottomSheet<WebLogin>(
        context: context,
        isScrollControlled: true,
        showDragHandle: true,
        shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
        builder: (_) => _ApproveSheet(merchant: widget.merchant, code: code, login: login),
      );
      if (approved != null && mounted) {
        _code.clear();
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(S.of(context).t('web_signed_in', {'browser': approved.browser}))));
        _loadSessions();
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _error = _problemText(p));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _signOut(WebSession w) async {
    try {
      await AppScope.read(context).api.signOutWebSession(w.id);
    } on ApiProblem catch (p) {
      if (mounted) ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(_problemText(p))));
    }
    _loadSessions();
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final ready = WebCodeFormatter.clean(_code.text).length == 8;
    return Scaffold(
      appBar: AppBar(title: Text(s.t('web_title'))),
      body: SafeArea(
        child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
          Text(s.t('web_steps', {'shop': widget.merchant.tradeName}), style: text.bodyMedium),
          const SizedBox(height: G1.s4),
          TextField(
            controller: _code,
            autofocus: true,
            textCapitalization: TextCapitalization.characters,
            autocorrect: false,
            enableSuggestions: false,
            inputFormatters: [WebCodeFormatter()],
            textAlign: TextAlign.center,
            style: text.headlineMedium?.copyWith(letterSpacing: 4, fontFeatures: G1.tabular),
            decoration: InputDecoration(labelText: s.t('web_code'), hintText: 'ABCD-EFGH', errorText: _error),
            onChanged: (_) => setState(() => _error = null),
            onSubmitted: (_) => ready && !_busy ? _continue() : null,
          ),
          const SizedBox(height: G1.s3),
          PrimaryButton(label: s.t('continue'), busy: _busy, onPressed: ready ? _continue : null),
          const SizedBox(height: G1.s3),
          Container(
            padding: const EdgeInsets.all(G1.s3),
            decoration: BoxDecoration(color: c.warningTint, borderRadius: BorderRadius.circular(G1.rM)),
            child: Text(s.t('web_safety'), style: text.bodySmall),
          ),
          const SizedBox(height: G1.s6),
          Text(s.t('web_signed_in_list').toUpperCase(), style: text.labelSmall),
          const SizedBox(height: G1.s2),
          if (_sessions == null)
            const Center(child: CircularProgressIndicator())
          else if (_sessions!.isEmpty)
            Text(s.t('web_none'), style: text.bodyMedium)
          else
            Container(
              decoration: BoxDecoration(
                  color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
              clipBehavior: Clip.antiAlias,
              child: Column(children: [
                for (var i = 0; i < _sessions!.length; i++) ...[
                  if (i > 0) const Divider(indent: G1.s4),
                  ListTile(
                    leading: const Icon(Icons.computer_outlined),
                    title: Text(_sessions![i].browser),
                    subtitle: Text('${_sessions![i].shopName} · ${formatWhen(context, _sessions![i].lastSeenAt)}'),
                    trailing: TextButton(onPressed: () => _signOut(_sessions![i]), child: Text(s.t('web_sign_out'))),
                  ),
                ],
              ]),
            ),
        ]),
      ),
    );
  }
}

class _ApproveSheet extends StatefulWidget {
  const _ApproveSheet({required this.merchant, required this.code, required this.login});
  final Merchant merchant;
  final String code;
  final WebLogin login;

  @override
  State<_ApproveSheet> createState() => _ApproveSheetState();
}

class _ApproveSheetState extends State<_ApproveSheet> {
  bool _busy = false;
  String? _error;

  Future<void> _approve(String pin) async {
    final api = AppScope.read(context).api;
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final done = await api.approveWebLogin(widget.merchant.id, widget.code, pin);
      if (mounted) Navigator.of(context).pop(done);
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _error = p.message.isNotEmpty ? p.message : S.of(context).t('error_generic'));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final l = widget.login;
    return SafeArea(
      child: SingleChildScrollView(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
        child: Column(mainAxisSize: MainAxisSize.min, children: [
          Text(s.t('web_approve_title', {'shop': widget.merchant.tradeName}),
              style: text.titleLarge, textAlign: TextAlign.center),
          const SizedBox(height: G1.s3),
          Container(
            width: double.infinity,
            padding: const EdgeInsets.all(G1.s4),
            decoration: BoxDecoration(color: c.surface2, borderRadius: BorderRadius.circular(G1.rL)),
            child: Row(children: [
              Icon(Icons.computer_outlined, color: c.inkMuted),
              const SizedBox(width: G1.s3),
              Expanded(
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  Text(l.browser, style: text.titleSmall),
                  Text([if (l.ip.isNotEmpty) l.ip, formatWhen(context, l.createdAt)].join(' · '),
                      style: text.bodySmall),
                ]),
              ),
            ]),
          ),
          const SizedBox(height: G1.s2),
          Text(s.t('web_approve_check'), style: text.bodySmall, textAlign: TextAlign.center),
          const SizedBox(height: G1.s5),
          PinPad(onComplete: _approve, error: _error, busy: _busy),
        ]),
      ),
    );
  }
}
