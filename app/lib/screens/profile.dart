import 'package:flutter/material.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/phone.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';

class ProfileScreen extends StatelessWidget {
  const ProfileScreen({super.key});

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final app = AppScope.of(context);
    final p = app.profile;
    Widget card(List<Widget> children) => Container(
          decoration: BoxDecoration(
              color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
          clipBehavior: Clip.antiAlias,
          child: Column(children: [
            for (var i = 0; i < children.length; i++) ...[if (i > 0) const Divider(indent: G1.s4), children[i]],
          ]),
        );
    Widget limitRow(String label, String value) => Padding(
          padding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s3),
          child: Row(children: [
            Expanded(child: Text(label, style: text.bodyMedium)),
            Text(value, style: text.titleSmall?.copyWith(fontFeatures: G1.tabular)),
          ]),
        );
    return SafeArea(
      child: ListView(padding: const EdgeInsets.fromLTRB(G1.s4, G1.s6, G1.s4, G1.s6), children: [
        Text(s.t('profile_title'), style: text.displayMedium),
        const SizedBox(height: G1.s6),
        if (p != null) ...[
          Row(children: [
            CircleAvatar(
              radius: 28,
              backgroundColor: c.brandTint,
              child: Text(p.displayName.isEmpty ? '•' : p.displayName.characters.first,
                  style: text.titleLarge?.copyWith(color: c.brand)),
            ),
            const SizedBox(width: G1.s4),
            Expanded(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                if (p.displayName.isNotEmpty) Text(p.displayName, style: text.titleMedium),
                Text(displayPhone(p.phone), style: text.bodySmall),
                Text(s.t('kyc_level', {'level': p.kycLevel}), style: text.bodySmall),
              ]),
            ),
          ]),
          const SizedBox(height: G1.s6),
          card([
            ListTile(
              leading: const Icon(Icons.badge_outlined),
              title: Text(s.t('display_name')),
              subtitle: p.displayName.isEmpty ? null : Text(p.displayName),
              trailing: const Icon(Icons.chevron_right),
              onTap: () =>
                  _editText(context, s.t('display_name'), p.displayName, (v) => app.api.updateMe(displayName: v)),
            ),
            ListTile(
              leading: const Icon(Icons.mail_outline),
              title: Text(s.t('email')),
              subtitle: p.email == null ? null : Text(p.email!),
              trailing: const Icon(Icons.chevron_right),
              onTap: () => _editText(context, s.t('email'), p.email ?? '', (v) => app.api.updateMe(email: v),
                  keyboard: TextInputType.emailAddress),
            ),
            ListTile(
              leading: const Icon(Icons.language),
              title: Text(s.t('language')),
              subtitle: Text(S.languageNames[app.locale.languageCode] ?? ''),
              trailing: const Icon(Icons.chevron_right),
              onTap: () => _pickLanguage(context),
            ),
          ]),
          const SizedBox(height: G1.s6),
          Text(s.t('limits').toUpperCase(), style: text.labelSmall),
          const SizedBox(height: G1.s2),
          if (p.limits == null)
            Text(s.t('limits_none'), style: text.bodySmall)
          else
            card([
              limitRow(s.t('limit_daily'), p.limits!.dailyRemaining.format()),
              limitRow(s.t('limit_monthly'), p.limits!.monthlyRemaining.format()),
              limitRow(s.t('limit_balance'), p.limits!.maxBalance.format()),
            ]),
          const SizedBox(height: G1.s6),
          card([
            ListTile(
              leading: const Icon(Icons.password),
              title: Text(s.t('change_pin')),
              trailing: const Icon(Icons.chevron_right),
              onTap: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => const ChangePinScreen())),
            ),
          ]),
        ],
        const SizedBox(height: G1.s6),
        OutlinedButton.icon(
          onPressed: app.signOut,
          style: OutlinedButton.styleFrom(foregroundColor: c.danger),
          icon: const Icon(Icons.logout),
          label: Text(s.t('sign_out')),
        ),
      ]),
    );
  }

  Future<void> _editText(BuildContext context, String label, String initial, Future<Profile> Function(String) save,
      {TextInputType keyboard = TextInputType.text}) async {
    final app = AppScope.read(context);
    final controller = TextEditingController(text: initial);
    final s = S.of(context);
    await showModalBottomSheet<void>(
      context: context,
      isScrollControlled: true,
      showDragHandle: true,
      shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
      builder: (sheet) {
        ApiProblem? problem;
        var busy = false;
        return StatefulBuilder(
          builder: (sheet, setSheet) => Padding(
            padding: EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4 + MediaQuery.of(sheet).viewInsets.bottom),
            child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
              TextField(
                controller: controller,
                autofocus: true,
                keyboardType: keyboard,
                maxLength: keyboard == TextInputType.emailAddress ? 254 : 60,
                decoration: InputDecoration(labelText: label, counterText: ''),
              ),
              if (problem != null) ...[const SizedBox(height: G1.s3), ProblemBanner(problem!)],
              const SizedBox(height: G1.s4),
              PrimaryButton(
                label: s.t('save'),
                busy: busy,
                onPressed: () async {
                  setSheet(() {
                    busy = true;
                    problem = null;
                  });
                  try {
                    final p = await save(controller.text);
                    app.profile = p;
                    await app.refresh();
                    if (sheet.mounted) Navigator.of(sheet).pop();
                  } on ApiProblem catch (e) {
                    setSheet(() {
                      problem = e;
                      busy = false;
                    });
                  }
                },
              ),
            ]),
          ),
        );
      },
    );
    controller.dispose();
  }

  Future<void> _pickLanguage(BuildContext context) {
    final app = AppScope.read(context);
    return showModalBottomSheet<void>(
      context: context,
      showDragHandle: true,
      shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
      builder: (sheet) => SafeArea(
        child: Column(mainAxisSize: MainAxisSize.min, children: [
          for (final e in S.languageNames.entries)
            ListTile(
              title: Text(e.value),
              trailing: app.locale.languageCode == e.key ? Icon(Icons.check, color: G1.c(sheet).brand) : null,
              onTap: () {
                app.setLocale(Locale(e.key));
                Navigator.of(sheet).pop();
              },
            ),
        ]),
      ),
    );
  }
}

/// Current PIN, then the new PIN twice.
class ChangePinScreen extends StatefulWidget {
  const ChangePinScreen({super.key});

  @override
  State<ChangePinScreen> createState() => _ChangePinScreenState();
}

class _ChangePinScreenState extends State<ChangePinScreen> {
  String? _current, _next;
  String? _error;
  bool _busy = false;

  Future<void> _entered(String pin) async {
    final s = S.of(context);
    if (_current == null) {
      setState(() {
        _current = pin;
        _error = null;
      });
      return;
    }
    if (_next == null) {
      setState(() => _next = pin);
      return;
    }
    if (pin != _next) {
      setState(() {
        _next = null;
        _error = s.t('pin_mismatch');
      });
      return;
    }
    setState(() => _busy = true);
    try {
      await AppScope.read(context).api.setPin(pin, currentPin: _current);
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(s.t('pin_changed'))));
      Navigator.of(context).pop();
    } on ApiProblem catch (p) {
      setState(() {
        _current = null;
        _next = null;
        _error = p.message.isNotEmpty ? p.message : s.t('error_generic');
      });
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final step = _current == null ? 'current_pin' : (_next == null ? 'new_pin' : 'pin_repeat_title');
    return Scaffold(
      appBar: AppBar(title: Text(s.t('change_pin'))),
      body: SafeArea(
        child: Padding(
          padding: const EdgeInsets.all(G1.s4),
          child: Column(children: [
            const SizedBox(height: G1.s6),
            Text(s.t(step), style: Theme.of(context).textTheme.titleLarge),
            const Spacer(),
            PinPad(key: ValueKey(step), onComplete: _entered, error: _error, busy: _busy),
          ]),
        ),
      ),
    );
  }
}
