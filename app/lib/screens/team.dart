import 'package:flutter/material.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/business.dart';
import '../core/phone.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';

String _date(DateTime t) => '${t.day.toString().padLeft(2, '0')}/${t.month.toString().padLeft(2, '0')}/${t.year}';

String _problem(BuildContext context, ApiProblem p) =>
    p.message.isNotEmpty ? p.message : S.of(context).t('error_generic');

Future<T?> _sheet<T>(BuildContext context, Widget child) => showModalBottomSheet<T>(
      context: context,
      isScrollControlled: true,
      showDragHandle: true,
      shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
      builder: (_) => child,
    );

Future<bool> _confirm(BuildContext context, String title, String body, String action) async {
  final s = S.of(context);
  final ok = await showDialog<bool>(
    context: context,
    builder: (ctx) => AlertDialog(
      title: Text(title),
      content: Text(body),
      actions: [
        TextButton(onPressed: () => Navigator.of(ctx).pop(false), child: Text(s.t('cancel'))),
        FilledButton(onPressed: () => Navigator.of(ctx).pop(true), child: Text(action)),
      ],
    ),
  );
  return ok ?? false;
}

/// Asks, then takes the signed-in person off the shop's team. Returns true when they left.
Future<bool> leaveShop(BuildContext context, Merchant m) async {
  final s = S.of(context);
  final api = AppScope.read(context).api;
  final messenger = ScaffoldMessenger.of(context);
  if (!await _confirm(context, s.t('team_leave', {'shop': m.tradeName}),
      s.t('team_leave_confirm', {'shop': m.tradeName}), s.t('team_leave_action'))) {
    return false;
  }
  try {
    await api.removeMember(m.id, api.api.userId!);
    messenger.showSnackBar(SnackBar(content: Text(s.t('team_left', {'shop': m.tradeName}))));
    return true;
  } on ApiProblem catch (p) {
    if (context.mounted) messenger.showSnackBar(SnackBar(content: Text(_problem(context, p))));
    return false;
  }
}

/// Who works at the shop. Owners and managers see it; owners invite, change roles and remove people.
class TeamScreen extends StatefulWidget {
  const TeamScreen(this.merchant, {super.key});
  final Merchant merchant;

  @override
  State<TeamScreen> createState() => _TeamScreenState();
}

class _TeamScreenState extends State<TeamScreen> {
  Team? _team;
  ApiProblem? _problemState;

  bool get _owner => widget.merchant.isOwner;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    try {
      final t = await AppScope.read(context).api.team(widget.merchant.id);
      if (mounted) {
        setState(() {
          _team = t;
          _problemState = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problemState = p);
    }
  }

  void _say(String text) => ScaffoldMessenger.of(context)
    ..hideCurrentSnackBar()
    ..showSnackBar(SnackBar(content: Text(text)));

  Future<void> _invite() async {
    final sent = await _sheet<TeamInvitation>(context, _InviteSheet(merchantId: widget.merchant.id));
    if (sent != null && mounted) {
      _say(S.of(context).t('team_sent', {'phone': displayPhone(sent.phone ?? '')}));
      _load();
    }
  }

  Future<void> _member(TeamMember m) async {
    final changed = await _sheet<String>(context, _MemberSheet(merchant: widget.merchant, member: m));
    if (changed != null && mounted) {
      _say(S.of(context).t(changed == 'removed' ? 'team_removed' : 'team_role_saved'));
      _load();
    }
  }

  Future<void> _cancel(TeamInvitation i) async {
    final s = S.of(context);
    try {
      await AppScope.read(context).api.cancelInvitation(widget.merchant.id, i.id);
      if (mounted) _say(s.t('team_invite_cancelled'));
    } on ApiProblem catch (p) {
      if (mounted) _say(_problem(context, p));
    }
    _load();
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final t = _team;
    Widget card(List<Widget> children) => Container(
          decoration: BoxDecoration(
              color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
          clipBehavior: Clip.antiAlias,
          child: Column(children: [
            for (var i = 0; i < children.length; i++) ...[if (i > 0) const Divider(indent: G1.s4), children[i]],
          ]),
        );
    return Scaffold(
      appBar: AppBar(title: Text(s.t('team_title'))),
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
            if (_problemState != null) ProblemBanner(_problemState!, onRetry: _load),
            if (t == null && _problemState == null) const Center(child: CircularProgressIndicator()),
            if (t != null) ...[
              Text(s.t('team_sub', {'shop': widget.merchant.tradeName}), style: text.bodyMedium),
              const SizedBox(height: G1.s4),
              card([
                for (final m in t.members)
                  ListTile(
                    leading: CircleAvatar(
                      backgroundColor: c.surface2,
                      child: Text((m.name.isNotEmpty ? m.name : '#').characters.first, style: text.titleSmall),
                    ),
                    title: Text([m.name.isNotEmpty ? m.name : m.phone, if (m.you) '(${s.t('you')})'].join(' ')),
                    subtitle: Text('${s.t('role_${m.role}')} · ${m.phone}'),
                    trailing: _owner && !m.you && m.role != 'owner' ? const Icon(Icons.chevron_right) : null,
                    onTap: _owner && !m.you && m.role != 'owner' ? () => _member(m) : null,
                  ),
              ]),
              const SizedBox(height: G1.s2),
              Text(s.t('team_owner_note'), style: text.bodySmall),
              if (_owner) ...[
                const SizedBox(height: G1.s5),
                PrimaryButton(label: s.t('team_invite'), onPressed: _invite),
                const SizedBox(height: G1.s6),
                Text(s.t('team_open_invites').toUpperCase(), style: text.labelSmall),
                const SizedBox(height: G1.s2),
                if (t.invitations.isEmpty)
                  Text(s.t('team_no_invites'), style: text.bodySmall)
                else
                  card([
                    for (final i in t.invitations)
                      ListTile(
                        title: Text(displayPhone(i.phone ?? '')),
                        subtitle:
                            Text('${s.t('role_${i.role}')} · ${s.t('team_expires', {'date': _date(i.expiresAt)})}'),
                        trailing: TextButton(onPressed: () => _cancel(i), child: Text(s.t('cancel'))),
                      ),
                  ]),
              ],
              const SizedBox(height: G1.s6),
              Text(s.t('team_roles').toUpperCase(), style: text.labelSmall),
              const SizedBox(height: G1.s2),
              for (final r in teamRoles)
                Padding(
                  padding: const EdgeInsets.only(bottom: G1.s2),
                  child: Text.rich(TextSpan(children: [
                    TextSpan(text: '${s.t('role_$r')}: ', style: text.titleSmall),
                    TextSpan(text: s.t('role_${r}_can'), style: text.bodyMedium),
                  ])),
                ),
            ],
          ]),
        ),
      ),
    );
  }
}

class _RolePicker extends StatelessWidget {
  const _RolePicker({required this.value, required this.onChanged});
  final String value;
  final ValueChanged<String> onChanged;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    return Column(children: [
      for (final r in teamRoles)
        RadioListTile<String>(
          contentPadding: EdgeInsets.zero,
          value: r,
          groupValue: value,
          onChanged: (v) => onChanged(v!),
          title: Text(s.t('role_$r')),
          subtitle: Text(s.t('role_${r}_can')),
        ),
    ]);
  }
}

class _InviteSheet extends StatefulWidget {
  const _InviteSheet({required this.merchantId});
  final String merchantId;

  @override
  State<_InviteSheet> createState() => _InviteSheetState();
}

class _InviteSheetState extends State<_InviteSheet> {
  final _phone = TextEditingController();
  String _role = 'cashier';
  bool _busy = false;
  String? _error;

  @override
  void dispose() {
    _phone.dispose();
    super.dispose();
  }

  Future<void> _send() async {
    final s = S.of(context);
    final phone = normalizePhone(_phone.text);
    if (phone == null) {
      setState(() => _error = s.t('phone_invalid'));
      return;
    }
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final inv = await AppScope.read(context).api.invite(widget.merchantId, phone, _role);
      if (mounted) Navigator.of(context).pop(inv);
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _error = _problem(context, p));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    return Padding(
      padding: EdgeInsets.only(bottom: MediaQuery.of(context).viewInsets.bottom),
      child: SafeArea(
        child: SingleChildScrollView(
          padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
          child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
            Text(s.t('team_invite'), style: text.titleLarge),
            const SizedBox(height: G1.s3),
            TextField(
              controller: _phone,
              keyboardType: TextInputType.phone,
              autofillHints: const [AutofillHints.telephoneNumber],
              decoration: InputDecoration(labelText: s.t('team_phone'), hintText: '09 11 23 45 67'),
            ),
            const SizedBox(height: G1.s3),
            _RolePicker(value: _role, onChanged: (r) => setState(() => _role = r)),
            Text(s.t('team_invite_help'), style: text.bodySmall),
            if (_error != null) ...[
              const SizedBox(height: G1.s2),
              Text(_error!, style: text.bodyMedium?.copyWith(color: c.danger)),
            ],
            const SizedBox(height: G1.s3),
            PrimaryButton(label: s.t('team_send'), busy: _busy, onPressed: _send),
          ]),
        ),
      ),
    );
  }
}

/// Change a member's role or remove them. Pops 'role' or 'removed' when something changed.
class _MemberSheet extends StatefulWidget {
  const _MemberSheet({required this.merchant, required this.member});
  final Merchant merchant;
  final TeamMember member;

  @override
  State<_MemberSheet> createState() => _MemberSheetState();
}

class _MemberSheetState extends State<_MemberSheet> {
  late String _role = widget.member.role;
  bool _busy = false;
  String? _error;

  Future<void> _run(Future<void> Function() f, String result) async {
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      await f();
      if (mounted) Navigator.of(context).pop(result);
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _error = _problem(context, p));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _remove() async {
    final s = S.of(context);
    final m = widget.member;
    final name = m.name.isNotEmpty ? m.name : m.phone;
    if (!await _confirm(context, s.t('team_remove'),
        s.t('team_remove_confirm', {'name': name, 'shop': widget.merchant.tradeName}), s.t('team_remove'))) {
      return;
    }
    if (!mounted) return;
    final api = AppScope.read(context).api;
    await _run(() => api.removeMember(widget.merchant.id, m.userId), 'removed');
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final m = widget.member;
    final api = AppScope.read(context).api;
    return SafeArea(
      child: SingleChildScrollView(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
        child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
          Text(m.name.isNotEmpty ? m.name : m.phone, style: text.titleLarge),
          Text('${m.phone} · ${s.t('team_since', {'date': _date(m.since)})}', style: text.bodySmall),
          const SizedBox(height: G1.s3),
          _RolePicker(value: _role, onChanged: (r) => setState(() => _role = r)),
          if (_error != null) Text(_error!, style: text.bodyMedium?.copyWith(color: c.danger)),
          const SizedBox(height: G1.s3),
          PrimaryButton(
            label: s.t('team_save_role'),
            busy: _busy,
            onPressed: _role == m.role
                ? null
                : () => _run(() => api.setMemberRole(widget.merchant.id, m.userId, _role), 'role'),
          ),
          const SizedBox(height: G1.s2),
          TextButton(
            onPressed: _busy ? null : _remove,
            style: TextButton.styleFrom(foregroundColor: c.danger),
            child: Text(s.t('team_remove')),
          ),
        ]),
      ),
    );
  }
}

/// Invitations to join shops' teams, sent to the signed-in person's number. Accepting pops the shop.
class InvitationsScreen extends StatefulWidget {
  const InvitationsScreen({super.key});

  @override
  State<InvitationsScreen> createState() => _InvitationsScreenState();
}

class _InvitationsScreenState extends State<InvitationsScreen> {
  List<TeamInvitation>? _list;
  ApiProblem? _problemState;
  String? _busy;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    try {
      final l = await AppScope.read(context).api.myInvitations();
      if (mounted) {
        setState(() {
          _list = l;
          _problemState = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problemState = p);
    }
  }

  Future<void> _answer(TeamInvitation i, bool accept) async {
    final s = S.of(context);
    final nav = Navigator.of(context);
    final messenger = ScaffoldMessenger.of(context);
    setState(() => _busy = i.id);
    try {
      final m = await AppScope.read(context).api.answerInvitation(i.id, accept: accept);
      if (m != null) {
        nav.pop(m);
        return;
      }
      messenger.showSnackBar(SnackBar(content: Text(s.t('invitation_declined'))));
    } on ApiProblem catch (p) {
      if (mounted) messenger.showSnackBar(SnackBar(content: Text(_problem(context, p))));
    } finally {
      if (mounted) setState(() => _busy = null);
    }
    _load();
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final list = _list;
    return Scaffold(
      appBar: AppBar(title: Text(s.t('invitations_title'))),
      body: SafeArea(
        child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
          if (_problemState != null) ProblemBanner(_problemState!, onRetry: _load),
          if (list == null && _problemState == null) const Center(child: CircularProgressIndicator()),
          if (list != null && list.isEmpty) Text(s.t('invitations_none'), style: text.bodyMedium),
          for (final i in list ?? const <TeamInvitation>[])
            Container(
              margin: const EdgeInsets.only(bottom: G1.s3),
              padding: const EdgeInsets.all(G1.s4),
              decoration: BoxDecoration(
                  color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
              child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
                Text(i.shop, style: text.titleMedium),
                const SizedBox(height: G1.s1),
                Text(s.t('invitation_body', {'name': i.invitedBy, 'role': s.t('role_${i.role}')}),
                    style: text.bodyMedium),
                Text(s.t('role_${i.role}_can'), style: text.bodySmall),
                Text(s.t('invitation_answer_by', {'date': _date(i.expiresAt)}), style: text.bodySmall),
                const SizedBox(height: G1.s3),
                Row(children: [
                  Expanded(
                    child: OutlinedButton(
                      onPressed: _busy == null ? () => _answer(i, false) : null,
                      child: Text(s.t('decline')),
                    ),
                  ),
                  const SizedBox(width: G1.s3),
                  Expanded(
                    child: FilledButton(
                      onPressed: _busy == null ? () => _answer(i, true) : null,
                      child: Text(s.t('accept')),
                    ),
                  ),
                ]),
              ]),
            ),
        ]),
      ),
    );
  }
}
