import 'package:flutter/material.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/business.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';

/// A phone signed in to the account.
class SignedInDevice {
  SignedInDevice(this.id, this.platform, this.model, this.firstSeen, this.lastActive, this.current, this.biometric);
  final String id;
  final String platform; // android, ios, web
  final String model;
  final DateTime firstSeen;
  final DateTime lastActive;
  final bool current;
  final bool biometric;

  String get name => model.isNotEmpty ? model : (platform == 'ios' ? 'iPhone' : 'Android');

  factory SignedInDevice.fromJson(Map<String, dynamic> j) => SignedInDevice(
        j['id'] as String,
        j['platform'] as String,
        j['model'] as String? ?? '',
        DateTime.parse(j['first_seen_at'] as String).toLocal(),
        DateTime.parse(j['last_active_at'] as String).toLocal(),
        j['current'] as bool? ?? false,
        j['biometric'] as bool? ?? false,
      );
}

extension DevicesApi on GebeyaApi {
  Future<List<SignedInDevice>> devices() async {
    final j = await api.get('/v1/me/devices');
    return [for (final x in j['items'] as List) SignedInDevice.fromJson(x as Map<String, dynamic>)];
  }

  Future<void> signOutDevice(String id, String pin) async => api.post('/v1/me/devices/$id/sign-out', {'pin': pin});

  /// Returns how many devices were signed out.
  Future<int> signOutOtherDevices(String pin) async =>
      ((await api.post('/v1/me/devices/sign-out-others', {'pin': pin}))['signed_out'] as num?)?.toInt() ?? 0;
}

String _date(DateTime t) => '${t.day.toString().padLeft(2, '0')}/${t.month.toString().padLeft(2, '0')}/${t.year}';

/// Phones signed in to the account, and computers signed in to the web business console. Other phones
/// are signed out with the PIN; this one signs out from the profile.
class DevicesScreen extends StatefulWidget {
  const DevicesScreen({super.key});

  @override
  State<DevicesScreen> createState() => _DevicesScreenState();
}

class _DevicesScreenState extends State<DevicesScreen> {
  List<SignedInDevice>? _devices;
  List<WebSession> _computers = const [];
  ApiProblem? _problem;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    final api = AppScope.read(context).api;
    try {
      final devices = await api.devices();
      var computers = const <WebSession>[];
      try {
        computers = await api.webSessions();
      } on ApiProblem {
        // the phones still show
      }
      if (mounted) {
        setState(() {
          _devices = devices;
          _computers = computers;
          _problem = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  void _say(String text) => ScaffoldMessenger.of(context)
    ..hideCurrentSnackBar()
    ..showSnackBar(SnackBar(content: Text(text)));

  Future<void> _signOut(SignedInDevice d) async {
    final s = S.of(context);
    final api = AppScope.read(context).api;
    final done = await _askPin(context, s.t('devices_sign_out_title', {'device': d.name}), s.t('devices_sign_out_body'),
        (pin) => api.signOutDevice(d.id, pin));
    if (done == true && mounted) {
      _say(s.t('devices_signed_out', {'device': d.name}));
      _load();
    }
  }

  Future<void> _signOutOthers() async {
    final s = S.of(context);
    final api = AppScope.read(context).api;
    var n = 0;
    final done = await _askPin(context, s.t('devices_sign_out_others'), s.t('devices_sign_out_others_body'),
        (pin) async => n = await api.signOutOtherDevices(pin));
    if (done == true && mounted) {
      _say(s.t('devices_signed_out_n', {'n': '$n'}));
      _load();
    }
  }

  Future<void> _signOutComputer(WebSession w) async {
    final s = S.of(context);
    try {
      await AppScope.read(context).api.signOutWebSession(w.id);
      if (mounted) _say(s.t('devices_signed_out', {'device': w.browser}));
    } on ApiProblem catch (p) {
      if (mounted) _say(p.message.isNotEmpty ? p.message : s.t('error_generic'));
    }
    _load();
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final list = _devices;
    final others = (list ?? const <SignedInDevice>[]).where((d) => !d.current).length;
    Widget card(List<Widget> children) => Container(
          decoration: BoxDecoration(
              color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
          clipBehavior: Clip.antiAlias,
          child: Column(children: [
            for (var i = 0; i < children.length; i++) ...[if (i > 0) const Divider(indent: G1.s4), children[i]],
          ]),
        );
    return Scaffold(
      appBar: AppBar(title: Text(s.t('devices_title'))),
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
            if (_problem != null) ProblemBanner(_problem!, onRetry: _load),
            if (list == null && _problem == null) const Center(child: CircularProgressIndicator()),
            if (list != null) ...[
              Text(s.t('devices_sub'), style: text.bodyMedium),
              const SizedBox(height: G1.s4),
              Text(s.t('devices_phones').toUpperCase(), style: text.labelSmall),
              const SizedBox(height: G1.s2),
              card([
                for (final d in list)
                  ListTile(
                    leading: Icon(d.platform == 'ios' ? Icons.phone_iphone : Icons.phone_android),
                    title: Text(d.name),
                    subtitle: Text([
                      d.current
                          ? s.t('devices_this_phone')
                          : s.t('devices_last_active', {'when': formatWhen(context, d.lastActive)}),
                      s.t('devices_since', {'date': _date(d.firstSeen)}),
                      if (d.biometric) s.t('devices_biometric'),
                    ].join(' · ')),
                    isThreeLine: d.biometric,
                    trailing: d.current
                        ? null
                        : TextButton(
                            onPressed: () => _signOut(d),
                            style: TextButton.styleFrom(foregroundColor: c.danger),
                            child: Text(s.t('web_sign_out')),
                          ),
                  ),
              ]),
              if (others > 1) ...[
                const SizedBox(height: G1.s3),
                OutlinedButton.icon(
                  onPressed: _signOutOthers,
                  style: OutlinedButton.styleFrom(foregroundColor: c.danger),
                  icon: const Icon(Icons.logout),
                  label: Text(s.t('devices_sign_out_others')),
                ),
              ],
              const SizedBox(height: G1.s2),
              Text(s.t('devices_not_you'), style: text.bodySmall),
              if (_computers.isNotEmpty) ...[
                const SizedBox(height: G1.s6),
                Text(s.t('web_signed_in_list').toUpperCase(), style: text.labelSmall),
                const SizedBox(height: G1.s2),
                card([
                  for (final w in _computers)
                    ListTile(
                      leading: const Icon(Icons.computer_outlined),
                      title: Text(w.browser),
                      subtitle: Text('${w.shopName} · ${formatWhen(context, w.lastSeenAt)}'),
                      trailing: TextButton(
                        onPressed: () => _signOutComputer(w),
                        style: TextButton.styleFrom(foregroundColor: c.danger),
                        child: Text(s.t('web_sign_out')),
                      ),
                    ),
                ]),
              ],
            ],
          ]),
        ),
      ),
    );
  }
}

/// Asks for the PIN, then runs [action] with it. Pops true when it succeeded; wrong PINs stay on the pad.
Future<bool?> _askPin(BuildContext context, String title, String body, Future<void> Function(String pin) action) =>
    showModalBottomSheet<bool>(
      context: context,
      isScrollControlled: true,
      showDragHandle: true,
      shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
      builder: (_) => _PinSheet(title: title, body: body, action: action),
    );

class _PinSheet extends StatefulWidget {
  const _PinSheet({required this.title, required this.body, required this.action});
  final String title;
  final String body;
  final Future<void> Function(String pin) action;

  @override
  State<_PinSheet> createState() => _PinSheetState();
}

class _PinSheetState extends State<_PinSheet> {
  bool _busy = false;
  String? _error;

  Future<void> _go(String pin) async {
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      await widget.action(pin);
      if (mounted) Navigator.of(context).pop(true);
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _error = p.message.isNotEmpty ? p.message : S.of(context).t('error_generic'));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final text = Theme.of(context).textTheme;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
        child: Column(mainAxisSize: MainAxisSize.min, children: [
          Text(widget.title, style: text.titleLarge, textAlign: TextAlign.center),
          const SizedBox(height: G1.s1),
          Text(widget.body, style: text.bodySmall, textAlign: TextAlign.center),
          const SizedBox(height: G1.s6),
          PinPad(onComplete: _go, error: _error, busy: _busy),
        ]),
      ),
    );
  }
}
