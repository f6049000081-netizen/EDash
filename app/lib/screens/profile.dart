import 'package:flutter/material.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/device_keys.dart';
import '../core/phone.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';
import 'business.dart';
import 'devices.dart';

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
          card([const BusinessTile()]),
          const SizedBox(height: G1.s6),
          card([
            const BiometricTile(),
            ListTile(
              leading: const Icon(Icons.devices_outlined),
              title: Text(s.t('devices_title')),
              subtitle: Text(s.t('devices_tile_sub')),
              trailing: const Icon(Icons.chevron_right),
              onTap: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => const DevicesScreen())),
            ),
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

/// "Confirm payments with fingerprint or face". Shown only on phones with a strong biometric sensor and
/// the native key support. Turning it on needs the PIN; the server then tells all the user's devices.
class BiometricTile extends StatefulWidget {
  const BiometricTile({super.key});

  @override
  State<BiometricTile> createState() => _BiometricTileState();
}

class _BiometricTileState extends State<BiometricTile> {
  bool? _available;
  bool _on = false;
  bool _busy = false;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    final keys = AppScope.read(context).api.api.keys;
    final available = await keys.biometricAvailable();
    final on = available && await keys.biometricEnrolled();
    if (!mounted) return;
    setState(() {
      _available = available;
      _on = on;
    });
  }

  Future<void> _toggle(bool on) async {
    final app = AppScope.read(context);
    final keys = app.api.api.keys;
    setState(() => _busy = true);
    try {
      if (!on) {
        try {
          await app.api.removeBiometric();
        } on ApiProblem {
          // the local key goes either way, so this phone can't confirm with it any more
        }
        await keys.biometricDelete();
        if (mounted) setState(() => _on = false);
        return;
      }
      final enrolled = await showModalBottomSheet<bool>(
        context: context,
        isScrollControlled: true,
        showDragHandle: true,
        shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
        builder: (_) => const _BiometricEnrollSheet(),
      );
      if (mounted) setState(() => _on = enrolled ?? false);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    if (_available != true) return const SizedBox.shrink();
    final s = S.of(context);
    return SwitchListTile(
      secondary: const Icon(Icons.fingerprint),
      title: Text(s.t('bio_setting')),
      subtitle: Text(s.t('bio_setting_body')),
      value: _on,
      onChanged: _busy ? null : _toggle,
    );
  }
}

class _BiometricEnrollSheet extends StatefulWidget {
  const _BiometricEnrollSheet();

  @override
  State<_BiometricEnrollSheet> createState() => _BiometricEnrollSheetState();
}

class _BiometricEnrollSheetState extends State<_BiometricEnrollSheet> {
  String? _error;
  bool _busy = false;

  Future<void> _enroll(String pin) async {
    final app = AppScope.read(context);
    final keys = app.api.api.keys;
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final jkt = await dpopThumbprint(keys);
      final key = await keys.biometricCreate(challenge: jkt == null ? null : biometricAttestChallenge(jkt));
      await app.api.enrollBiometric(key.publicKey, pin, attestation: key.chain);
      if (mounted) Navigator.of(context).pop(true);
    } on ApiProblem catch (p) {
      await keys.biometricDelete();
      if (mounted) setState(() => _error = p.message.isNotEmpty ? p.message : S.of(context).t('error_generic'));
    } catch (_) {
      await keys.biometricDelete();
      if (mounted) setState(() => _error = S.of(context).t('error_generic'));
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
          Text(s.t('bio_enroll_title'), style: text.titleLarge),
          const SizedBox(height: G1.s1),
          Text(s.t('bio_enroll_body'), style: text.bodySmall, textAlign: TextAlign.center),
          const SizedBox(height: G1.s6),
          PinPad(onComplete: _enroll, error: _error, busy: _busy),
        ]),
      ),
    );
  }
}
