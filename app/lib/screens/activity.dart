import 'package:flutter/material.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';

/// Full activity list with filters and cursor paging.
class ActivityScreen extends StatefulWidget {
  const ActivityScreen({super.key});

  @override
  State<ActivityScreen> createState() => _ActivityScreenState();
}

class _ActivityScreenState extends State<ActivityScreen> {
  String _filter = 'all';
  final List<ActivityItem> _items = [];
  String? _cursor;
  bool _loading = false;
  bool _loadedOnce = false;
  ApiProblem? _problem;
  int _loadedVersion = -1;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    final v = AppScope.of(context).activityVersion;
    if (v != _loadedVersion) {
      _loadedVersion = v;
      _reload();
    }
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
    final filter = _filter;
    try {
      final page = await AppScope.read(context).api.activity(filter: filter, cursor: _cursor);
      if (!mounted || filter != _filter) return;
      setState(() {
        _items.addAll(page.items);
        _cursor = page.nextCursor;
        _loadedOnce = true;
      });
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
    return SafeArea(
      child: RefreshIndicator(
        onRefresh: _reload,
        child: ListView(
          padding: const EdgeInsets.only(bottom: G1.s6),
          children: [
            Padding(
              padding: const EdgeInsets.fromLTRB(G1.s4, G1.s6, G1.s4, G1.s4),
              child: Text(s.t('activity_title'), style: text.displayMedium),
            ),
            Padding(
              padding: const EdgeInsets.symmetric(horizontal: G1.s4),
              child: SegmentedButton<String>(
                showSelectedIcon: false,
                segments: [
                  for (final f in const ['all', 'money', 'orders'])
                    ButtonSegment(value: f, label: Text(s.t('filter_$f'))),
                ],
                selected: {_filter},
                onSelectionChanged: (v) {
                  setState(() => _filter = v.first);
                  _reload();
                },
              ),
            ),
            const SizedBox(height: G1.s4),
            if (_items.isNotEmpty)
              Container(
                margin: const EdgeInsets.symmetric(horizontal: G1.s4),
                decoration: BoxDecoration(
                    color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
                clipBehavior: Clip.antiAlias,
                child: Column(children: [
                  for (var i = 0; i < _items.length; i++) ...[
                    if (i > 0) const Divider(indent: 68),
                    TransactionRow(item: _items[i]),
                  ],
                ]),
              ),
            if (_loadedOnce && _items.isEmpty && _problem == null)
              Padding(
                padding: const EdgeInsets.all(G1.s6),
                child: Text(s.t('no_activity'),
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
