import 'dart:async';

import 'package:flutter/material.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';
import 'activity.dart';
import 'notifications.dart';
import 'pay.dart';
import 'profile.dart';
import 'shop.dart';
import 'topup.dart';

/// Five tabs: Home, Explore, Pay (raised centre button), Activity, Profile.
class Shell extends StatefulWidget {
  const Shell({super.key});

  @override
  State<Shell> createState() => _ShellState();
}

class _ShellState extends State<Shell> with WidgetsBindingObserver {
  int _tab = 0;
  Timer? _poll;

  // The bell is polled every 30 seconds while the app is in the foreground, and right away on resume.
  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    _startPolling();
  }

  void _startPolling() {
    _poll?.cancel();
    _poll = Timer.periodic(const Duration(seconds: 30), (_) => AppScope.read(context).refreshNotifications());
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.resumed) {
      AppScope.read(context).refresh();
      _startPolling();
    } else if (state == AppLifecycleState.paused) {
      _poll?.cancel();
    }
  }

  @override
  void dispose() {
    _poll?.cancel();
    WidgetsBinding.instance.removeObserver(this);
    super.dispose();
  }

  void _openPay() => showPaySheet(context);

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final pages = [
      HomeTab(onSeeAll: () => setState(() => _tab = 3)),
      const ExploreTab(),
      const SizedBox.shrink(),
      const ActivityScreen(),
      const ProfileScreen(),
    ];
    Widget item(int i, IconData icon, IconData active, String label) {
      final selected = _tab == i;
      return Expanded(
        child: Semantics(
          selected: selected,
          button: true,
          label: label,
          excludeSemantics: true,
          child: InkWell(
            onTap: () => setState(() => _tab = i),
            child: SizedBox(
              height: 64,
              child: Column(mainAxisAlignment: MainAxisAlignment.center, children: [
                Icon(selected ? active : icon, color: selected ? c.brand : c.inkSubtle),
                const SizedBox(height: 2),
                Text(label,
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    style: Theme.of(context)
                        .textTheme
                        .labelSmall
                        ?.copyWith(color: selected ? c.brand : c.inkMuted, letterSpacing: 0)),
              ]),
            ),
          ),
        ),
      );
    }

    return Scaffold(
      body: IndexedStack(index: _tab, children: pages),
      bottomNavigationBar: Container(
        decoration: BoxDecoration(color: c.surface1, border: Border(top: BorderSide(color: c.line))),
        child: SafeArea(
          top: false,
          child: Row(children: [
            item(0, Icons.home_outlined, Icons.home, s.t('tab_home')),
            item(1, Icons.explore_outlined, Icons.explore, s.t('tab_explore')),
            Expanded(
              child: SizedBox(
                height: 64,
                child: Center(
                  child: Semantics(
                    button: true,
                    label: s.t('tab_pay'),
                    excludeSemantics: true,
                    child: GestureDetector(
                      onTap: _openPay,
                      child: Container(
                        width: 56,
                        height: 56,
                        decoration:
                            BoxDecoration(color: c.brand, shape: BoxShape.circle, boxShadow: G1.shadow2(context)),
                        child: Icon(Icons.qr_code_scanner, color: c.onBrand),
                      ),
                    ),
                  ),
                ),
              ),
            ),
            item(3, Icons.receipt_long_outlined, Icons.receipt_long, s.t('tab_activity')),
            item(4, Icons.person_outline, Icons.person, s.t('tab_profile')),
          ]),
        ),
      ),
    );
  }
}

class HomeTab extends StatefulWidget {
  const HomeTab({super.key, required this.onSeeAll});
  final VoidCallback onSeeAll;

  @override
  State<HomeTab> createState() => _HomeTabState();
}

class _HomeTabState extends State<HomeTab> {
  List<ActivityItem>? _recent;
  ApiProblem? _problem;
  int _loadedVersion = -1;

  Future<void> _load() async {
    final app = AppScope.read(context);
    _loadedVersion = app.activityVersion;
    try {
      final page = await app.api.activity(limit: 5);
      if (mounted) {
        setState(() {
          _recent = page.items;
          _problem = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (AppScope.of(context).activityVersion != _loadedVersion) _load();
  }

  String _greeting(S s) {
    final h = DateTime.now().hour;
    return s.t(h < 12 ? 'greeting_morning' : (h < 18 ? 'greeting_afternoon' : 'greeting_evening'));
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final app = AppScope.of(context);
    final name = app.profile?.displayName ?? '';
    return SafeArea(
      child: RefreshIndicator(
        onRefresh: app.refresh,
        child: ListView(
          padding: const EdgeInsets.fromLTRB(G1.s4, G1.s6, G1.s4, G1.s6),
          children: [
            Row(children: [
              Expanded(
                child: Text(name.isEmpty ? _greeting(s) : '${_greeting(s)}, ${name.split(' ').first}',
                    style: text.displayMedium),
              ),
              NotificationBell(unread: app.unreadNotifications),
            ]),
            const SizedBox(height: G1.s6),
            BalanceCard(
              balance: app.balance,
              hidden: app.balanceHidden,
              onToggle: app.toggleBalance,
              onRetry: app.balance == null && app.profile != null ? app.refresh : null,
            ),
            const SizedBox(height: G1.s6),
            Row(children: [
              Expanded(
                  child:
                      QuickAction(icon: Icons.north_east, label: s.t('action_send'), onTap: () => openSend(context))),
              Expanded(
                  child: QuickAction(
                      icon: Icons.qr_code_scanner, label: s.t('action_scan'), onTap: () => openScan(context))),
              Expanded(child: QuickAction(icon: Icons.add, label: s.t('action_add'), onTap: () => openTopUp(context))),
            ]),
            const SizedBox(height: G1.s6),
            Row(children: [
              Expanded(child: Text(s.t('recent_activity'), style: text.labelSmall)),
              TextButton(onPressed: widget.onSeeAll, child: Text(s.t('see_all'))),
            ]),
            Container(
              decoration: BoxDecoration(
                  color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
              clipBehavior: Clip.antiAlias,
              child: _recentList(s, text),
            ),
          ],
        ),
      ),
    );
  }

  Widget _recentList(S s, TextTheme text) {
    if (_problem != null && _recent == null) {
      return Padding(padding: const EdgeInsets.all(G1.s4), child: ProblemBanner(_problem!, onRetry: _load));
    }
    if (_recent == null) {
      return const Padding(padding: EdgeInsets.all(G1.s6), child: Center(child: CircularProgressIndicator()));
    }
    if (_recent!.isEmpty) {
      return Padding(padding: const EdgeInsets.all(G1.s5), child: Text(s.t('no_activity'), style: text.bodySmall));
    }
    return Column(children: [
      for (var i = 0; i < _recent!.length; i++) ...[
        if (i > 0) const Divider(indent: 68),
        TransactionRow(item: _recent![i]),
      ],
    ]);
  }
}

/// Shops taking orders in the app, and the customer's own orders.
class ExploreTab extends StatefulWidget {
  const ExploreTab({super.key});

  @override
  State<ExploreTab> createState() => _ExploreTabState();
}

class _ExploreTabState extends State<ExploreTab> {
  final _list = GlobalKey<ShopListState>();

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    return SafeArea(
      child: RefreshIndicator(
        onRefresh: () async => _list.currentState?.load(),
        child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
          const SizedBox(height: G1.s2),
          Row(children: [
            Expanded(child: Text(s.t('explore_title'), style: text.displayMedium)),
            TextButton.icon(
              onPressed: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => const MyOrdersScreen())),
              icon: const Icon(Icons.receipt_outlined),
              label: Text(s.t('my_orders')),
            ),
          ]),
          const SizedBox(height: G1.s5),
          Text(s.t('shops').toUpperCase(), style: text.labelSmall),
          const SizedBox(height: G1.s2),
          ShopList(key: _list),
        ]),
      ),
    );
  }
}
