import 'package:flutter/material.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';

/// The bell on the home screen, with the unread count.
class NotificationBell extends StatelessWidget {
  const NotificationBell({super.key, required this.unread});

  final int unread;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final label =
        unread > 0 ? '${s.t('notifications')}, ${s.t('notifications_unread', {'n': '$unread'})}' : s.t('notifications');
    return IconButton(
      tooltip: label,
      onPressed: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => const NotificationsScreen())),
      icon: Badge(
        isLabelVisible: unread > 0,
        label: Text(unread > 99 ? '99+' : '$unread'),
        child: Icon(unread > 0 ? Icons.notifications : Icons.notifications_none),
      ),
    );
  }
}

/// Newest first. Opening the list marks everything read; items that were unread keep their dot
/// until the screen closes, so the user can still see what's new.
class NotificationsScreen extends StatefulWidget {
  const NotificationsScreen({super.key});

  @override
  State<NotificationsScreen> createState() => _NotificationsScreenState();
}

class _NotificationsScreenState extends State<NotificationsScreen> {
  final List<AppNotification> _items = [];
  String? _cursor;
  bool _loading = false;
  bool _loadedOnce = false;
  ApiProblem? _problem;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _reload());
  }

  Future<void> _reload() async {
    _items.clear();
    _cursor = null;
    _loadedOnce = false;
    await _more();
  }

  Future<void> _more() async {
    if (_loading) return;
    setState(() {
      _loading = true;
      _problem = null;
    });
    final app = AppScope.read(context);
    try {
      final page = await app.api.notifications(cursor: _cursor);
      if (!mounted) return;
      final first = !_loadedOnce;
      setState(() {
        _items.addAll(page.items);
        _cursor = page.nextCursor;
        _loadedOnce = true;
      });
      if (first && page.unread > 0) await app.markNotificationsRead();
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    } finally {
      if (mounted) setState(() => _loading = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    return Scaffold(
      appBar: AppBar(title: Text(s.t('notifications'))),
      body: RefreshIndicator(
        onRefresh: _reload,
        child: ListView(
          padding: const EdgeInsets.all(G1.s4),
          children: [
            if (_items.isNotEmpty)
              Container(
                decoration: BoxDecoration(
                    color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
                clipBehavior: Clip.antiAlias,
                child: Column(children: [
                  for (var i = 0; i < _items.length; i++) ...[
                    if (i > 0) const Divider(indent: 68),
                    _NotificationRow(_items[i]),
                  ],
                ]),
              ),
            if (_loadedOnce && _items.isEmpty && _problem == null)
              Padding(
                padding: const EdgeInsets.all(G1.s6),
                child: Text(s.t('notifications_empty'),
                    textAlign: TextAlign.center, style: text.bodyMedium?.copyWith(color: c.inkMuted)),
              ),
            if (_problem != null)
              Padding(padding: const EdgeInsets.all(G1.s4), child: ProblemBanner(_problem!, onRetry: _more)),
            if (_loading)
              const Padding(padding: EdgeInsets.all(G1.s6), child: Center(child: CircularProgressIndicator()))
            else if (_cursor != null && _problem == null)
              Padding(
                padding: const EdgeInsets.all(G1.s4),
                child: OutlinedButton(onPressed: _more, child: Text(s.t('load_more'))),
              ),
          ],
        ),
      ),
    );
  }
}

class _NotificationRow extends StatelessWidget {
  const _NotificationRow(this.n);

  final AppNotification n;

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final (IconData icon, Color fg, Color bg) = switch (n.kind) {
      _ when n.isSecurity => (Icons.shield_outlined, c.danger, c.dangerTint),
      'money_received' || 'topup_succeeded' || 'payment_succeeded' => (Icons.south_west, c.success, c.successTint),
      'topup_held' => (Icons.hourglass_top, c.warning, c.warningTint),
      _ => (Icons.error_outline, c.danger, c.dangerTint),
    };
    return Semantics(
      container: true,
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s3),
        child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
          CircleAvatar(radius: 20, backgroundColor: bg, child: Icon(icon, color: fg, size: 20)),
          const SizedBox(width: G1.s3),
          Expanded(
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              Text(n.title, style: text.titleSmall),
              const SizedBox(height: 2),
              Text(n.body, style: text.bodyMedium),
              const SizedBox(height: 4),
              Text(formatWhen(context, n.createdAt), style: text.bodySmall?.copyWith(color: c.inkMuted)),
            ]),
          ),
          if (!n.read)
            Padding(
              padding: const EdgeInsets.only(left: G1.s2, top: 6),
              child: Container(width: 8, height: 8, decoration: BoxDecoration(color: c.brand, shape: BoxShape.circle)),
            ),
        ]),
      ),
    );
  }
}
