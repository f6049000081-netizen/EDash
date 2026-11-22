import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:image_picker/image_picker.dart';
import 'package:qr_flutter/qr_flutter.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/business.dart';
import '../core/phone.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';
import 'computer.dart';
import 'shop_admin.dart';

Future<T?> _sheet<T>(BuildContext context, Widget child) => showModalBottomSheet<T>(
      context: context,
      isScrollControlled: true,
      showDragHandle: true,
      shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
      builder: (_) => child,
    );

String _day(DateTime t) => '${t.day.toString().padLeft(2, '0')}/${t.month.toString().padLeft(2, '0')}/${t.year}';

/// The "Business" row on the profile screen: opens the shop, or the application form for people who
/// don't have one.
class BusinessTile extends StatefulWidget {
  const BusinessTile({super.key});

  @override
  State<BusinessTile> createState() => _BusinessTileState();
}

class _BusinessTileState extends State<BusinessTile> {
  List<Merchant>? _shops;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    try {
      final shops = await AppScope.read(context).api.myMerchants();
      if (mounted) setState(() => _shops = shops);
    } on ApiProblem {
      if (mounted) setState(() => _shops = const []);
    }
  }

  Future<void> _open() async {
    final shops = _shops ?? const [];
    final nav = Navigator.of(context);
    if (shops.isEmpty) {
      final applied = await nav.push<Merchant>(MaterialPageRoute(builder: (_) => const ApplyScreen()));
      if (applied != null) await nav.push(MaterialPageRoute(builder: (_) => BusinessScreen(applied.id)));
    } else if (shops.length == 1) {
      await nav.push(MaterialPageRoute(builder: (_) => BusinessScreen(shops.single.id)));
    } else {
      final pick = await _sheet<Merchant>(
        context,
        SafeArea(
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            for (final m in shops)
              ListTile(
                leading: const Icon(Icons.storefront_outlined),
                title: Text(m.tradeName),
                subtitle: Text(S.of(context).t('role_${m.role}')),
                onTap: () => Navigator.of(context).pop(m),
              ),
          ]),
        ),
      );
      if (pick != null) await nav.push(MaterialPageRoute(builder: (_) => BusinessScreen(pick.id)));
    }
    _load();
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final shops = _shops;
    return ListTile(
      leading: const Icon(Icons.storefront_outlined),
      title: Text(s.t('business')),
      subtitle: Text(shops == null
          ? ''
          : shops.isEmpty
              ? s.t('business_none')
              : shops.length == 1
                  ? shops.single.tradeName
                  : s.t('business_count', {'n': '${shops.length}'})),
      trailing: const Icon(Icons.chevron_right),
      onTap: shops == null ? null : _open,
    );
  }
}

// ---------------------------------------------------------------------------
// Applying
// ---------------------------------------------------------------------------

class ApplyScreen extends StatefulWidget {
  const ApplyScreen({super.key});

  @override
  State<ApplyScreen> createState() => _ApplyScreenState();
}

class _ApplyScreenState extends State<ApplyScreen> {
  final _form = GlobalKey<FormState>();
  final _trade = TextEditingController(), _legal = TextEditingController(), _tin = TextEditingController();
  final _licence = TextEditingController(), _city = TextEditingController();
  String? _category;
  bool _busy = false;
  ApiProblem? _problem;

  @override
  void dispose() {
    for (final c in [_trade, _legal, _tin, _licence, _city]) {
      c.dispose();
    }
    super.dispose();
  }

  Future<void> _submit() async {
    if (!_form.currentState!.validate()) return;
    setState(() {
      _busy = true;
      _problem = null;
    });
    try {
      final m = await AppScope.read(context).api.applyMerchant(
            legalName: _legal.text,
            tradeName: _trade.text,
            tin: _tin.text.trim(),
            licence: _licence.text.trim(),
            category: _category!,
            city: _city.text,
          );
      if (mounted) Navigator.of(context).pop(m);
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    String? required(String? v) => (v == null || v.trim().length < 2) ? s.t('field_required') : null;
    return Scaffold(
      appBar: AppBar(title: Text(s.t('apply_title'))),
      body: SafeArea(
        child: Form(
          key: _form,
          child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
            Text(s.t('apply_intro'), style: text.bodyMedium),
            const SizedBox(height: G1.s6),
            TextFormField(
              controller: _trade,
              maxLength: 60,
              textCapitalization: TextCapitalization.words,
              decoration: InputDecoration(labelText: s.t('apply_trade_name'), helperText: s.t('apply_trade_help')),
              validator: required,
            ),
            const SizedBox(height: G1.s3),
            TextFormField(
              controller: _legal,
              maxLength: 120,
              textCapitalization: TextCapitalization.words,
              decoration: InputDecoration(labelText: s.t('apply_legal_name'), helperText: s.t('apply_legal_help')),
              validator: required,
            ),
            const SizedBox(height: G1.s3),
            TextFormField(
              controller: _tin,
              keyboardType: TextInputType.number,
              inputFormatters: [FilteringTextInputFormatter.digitsOnly, LengthLimitingTextInputFormatter(10)],
              decoration: InputDecoration(labelText: s.t('apply_tin'), helperText: s.t('apply_tin_help')),
              validator: (v) => RegExp(r'^\d{10}$').hasMatch(v ?? '') ? null : s.t('apply_tin_help'),
            ),
            const SizedBox(height: G1.s3),
            TextFormField(
              controller: _licence,
              decoration: InputDecoration(labelText: s.t('apply_licence')),
            ),
            const SizedBox(height: G1.s3),
            DropdownButtonFormField<String>(
              value: _category,
              isExpanded: true,
              decoration: InputDecoration(labelText: s.t('apply_category')),
              items: [
                for (final c in merchantCategories) DropdownMenuItem(value: c, child: Text(s.t('cat_$c'))),
              ],
              onChanged: (v) => setState(() => _category = v),
              validator: (v) => v == null ? s.t('field_required') : null,
            ),
            const SizedBox(height: G1.s3),
            TextFormField(
              controller: _city,
              maxLength: 40,
              textCapitalization: TextCapitalization.words,
              decoration: InputDecoration(labelText: s.t('apply_city')),
              validator: required,
            ),
            const SizedBox(height: G1.s4),
            Text(s.t('apply_review_note'), style: text.bodySmall),
            if (_problem != null) ...[const SizedBox(height: G1.s4), ProblemBanner(_problem!)],
            const SizedBox(height: G1.s6),
            PrimaryButton(label: s.t('apply_submit'), busy: _busy, onPressed: _submit),
          ]),
        ),
      ),
    );
  }
}

// ---------------------------------------------------------------------------
// The shop
// ---------------------------------------------------------------------------

class BusinessScreen extends StatefulWidget {
  const BusinessScreen(this.merchantId, {super.key});
  final String merchantId;

  @override
  State<BusinessScreen> createState() => _BusinessScreenState();
}

class _BusinessScreenState extends State<BusinessScreen> with WidgetsBindingObserver {
  Merchant? _m;
  MerchantSummary? _summary;
  List<ShopPayment> _payments = const [];
  PayoutAccount? _account;
  bool _accountLoaded = false;
  ApiProblem? _problem;
  Timer? _poll;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
    // "Live payments": a cashier watches this to see that a customer has paid.
    _poll = Timer.periodic(const Duration(seconds: 15), (_) => _loadLive());
  }

  @override
  void dispose() {
    _poll?.cancel();
    WidgetsBinding.instance.removeObserver(this);
    super.dispose();
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.resumed) _loadLive();
  }

  Future<void> _load() async {
    final api = AppScope.read(context).api;
    try {
      final m = await api.merchant(widget.merchantId);
      if (!mounted) return;
      setState(() {
        _m = m;
        _problem = null;
      });
      if (m.active) await _loadLive();
      if (m.active && m.seesMoney) {
        final account = await api.payoutAccount(m.id);
        if (mounted) {
          setState(() {
            _account = account;
            _accountLoaded = true;
          });
        }
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  Future<void> _loadLive() async {
    final m = _m;
    if (m == null || !m.active) return;
    final api = AppScope.read(context).api;
    try {
      final results = await Future.wait([api.merchantSummary(m.id), api.shopPayments(m.id)]);
      if (mounted) {
        setState(() {
          _summary = results[0] as MerchantSummary;
          _payments = results[1] as List<ShopPayment>;
        });
      }
    } on ApiProblem {
      // keep what's on screen; the next poll tries again
    }
  }

  Future<void> _showQr() async {
    final m = _m!;
    final s = S.of(context);
    try {
      final qr = await AppScope.read(context).api.shopQr(m.id);
      if (!mounted) return;
      await _sheet<void>(context, _QrSheet(name: m.tradeName, payload: qr.payload, merchantNo: qr.merchantNo));
    } on ApiProblem catch (p) {
      if (mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(p.message.isNotEmpty ? p.message : s.t('error_generic'))));
      }
    }
  }

  Future<void> _changeAccount() async {
    final saved = await _sheet<PayoutAccount>(context, _PayoutAccountSheet(merchantId: _m!.id));
    if (saved != null && mounted) {
      setState(() => _account = saved);
      _loadLive();
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final m = _m;
    return Scaffold(
      appBar: AppBar(title: Text(m?.tradeName ?? s.t('business'))),
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
            if (_problem != null) ProblemBanner(_problem!, onRetry: _load),
            if (m == null && _problem == null) const Center(child: CircularProgressIndicator()),
            if (m != null)
              ...switch (m.status) {
                'active' => _active(context, m),
                'suspended' => [
                    _notice(
                        context, Icons.pause_circle_outline, s.t('shop_suspended_title'), s.t('shop_suspended_body'))
                  ],
                _ => [
                    _notice(context, Icons.hourglass_top, s.t('shop_review_title', {'shop': m.tradeName}),
                        s.t('shop_review_body')),
                    if (m.role == 'owner' || m.role == 'manager') ...[
                      const SizedBox(height: G1.s4),
                      DocumentsCard(merchant: m),
                    ],
                  ],
              },
          ]),
        ),
      ),
    );
  }

  Widget _notice(BuildContext context, IconData icon, String title, String body) {
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    return Container(
      padding: const EdgeInsets.all(G1.s4),
      decoration: BoxDecoration(color: c.brandTint, borderRadius: BorderRadius.circular(G1.rL)),
      child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Icon(icon, color: c.brand),
        const SizedBox(width: G1.s3),
        Expanded(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(title, style: text.titleMedium),
            const SizedBox(height: G1.s1),
            Text(body, style: text.bodyMedium),
          ]),
        ),
      ]),
    );
  }

  List<Widget> _active(BuildContext context, Merchant m) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final sum = _summary;
    Widget card(List<Widget> children) => Container(
          decoration: BoxDecoration(
              color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
          clipBehavior: Clip.antiAlias,
          child: Column(children: [
            for (var i = 0; i < children.length; i++) ...[if (i > 0) const Divider(indent: G1.s4), children[i]],
          ]),
        );
    return [
      Text(
          '${s.t('role_${m.role}')}${m.merchantNo == null ? '' : ' · ${s.t('merchant_no')} ${formatMerchantNo(m.merchantNo!)}'}',
          style: text.bodySmall),
      const SizedBox(height: G1.s3),
      // Sales today, with the QR button: the shop's main screen.
      Container(
        padding: const EdgeInsets.all(G1.s5),
        decoration: BoxDecoration(color: c.brandStrong, borderRadius: BorderRadius.circular(G1.rL)),
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Text(s.t('sales_today').toUpperCase(), style: text.labelSmall?.copyWith(color: c.onBrand.withOpacity(0.85))),
          const SizedBox(height: G1.s1),
          Semantics(
            label: sum?.salesToday.spoken(),
            excludeSemantics: true,
            child: Text(sum?.salesToday.format() ?? '…',
                style: text.displaySmall?.copyWith(color: c.onBrand, fontFeatures: G1.tabular)),
          ),
          if (sum != null)
            Text(
                s.t('payments_today', {'n': '${sum.paymentsToday}'}) +
                    (sum.paymentsToday > 0 ? ' · ${s.t('average')} ${sum.averageBasket.format()}' : ''),
                style: text.bodySmall?.copyWith(color: c.onBrand.withOpacity(0.85))),
          const SizedBox(height: G1.s4),
          SizedBox(
            width: double.infinity,
            child: FilledButton.icon(
              style: FilledButton.styleFrom(backgroundColor: c.accent, foregroundColor: c.onAccent),
              onPressed: _showQr,
              icon: const Icon(Icons.qr_code_2),
              label: Text(s.t('show_my_qr')),
            ),
          ),
        ]),
      ),
      const SizedBox(height: G1.s6),
      Text(s.t('online_orders').toUpperCase(), style: text.labelSmall),
      const SizedBox(height: G1.s2),
      card([
        ListTile(
          leading: const Icon(Icons.shopping_bag_outlined),
          title: Text(s.t('shop_orders')),
          subtitle: Text(s.t('shop_orders_sub')),
          trailing: const Icon(Icons.chevron_right),
          onTap: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => ShopOrdersScreen(m))),
        ),
        ListTile(
          leading: const Icon(Icons.inventory_2_outlined),
          title: Text(s.t('catalog')),
          subtitle: Text(s.t('catalog_sub')),
          trailing: const Icon(Icons.chevron_right),
          onTap: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => CatalogScreen(m))),
        ),
        ListTile(
          leading: const Icon(Icons.computer_outlined),
          title: Text(s.t('web_title')),
          subtitle: Text(s.t('web_tile_sub')),
          trailing: const Icon(Icons.chevron_right),
          onTap: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => ComputerSignInScreen(m))),
        ),
      ]),
      if (m.seesMoney) ...[
        const SizedBox(height: G1.s6),
        Text(s.t('payouts').toUpperCase(), style: text.labelSmall),
        const SizedBox(height: G1.s2),
        card([
          ListTile(
            leading: const Icon(Icons.account_balance_wallet_outlined),
            title: Text(s.t('next_payout')),
            subtitle: Text(_payoutLine(s, sum)),
            trailing: Text(sum?.next?.net.format() ?? sum?.pending?.format() ?? '',
                style: text.titleSmall?.copyWith(fontFeatures: G1.tabular)),
          ),
          ListTile(
            leading: const Icon(Icons.account_balance_outlined),
            title: Text(s.t('payout_account')),
            subtitle: Text(!_accountLoaded
                ? ''
                : _account == null
                    ? s.t('payout_account_none')
                    : _account!.payoutsFrom.isAfter(DateTime.now())
                        ? '${_account!.label} · ${s.t('payout_account_from', {'when': _day(_account!.payoutsFrom)})}'
                        : _account!.label),
            trailing: m.isOwner ? const Icon(Icons.chevron_right) : null,
            onTap: m.isOwner ? _changeAccount : null,
          ),
          ListTile(
            leading: const Icon(Icons.receipt_long_outlined),
            title: Text(s.t('payout_history')),
            trailing: const Icon(Icons.chevron_right),
            onTap: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => PayoutsScreen(m))),
          ),
        ]),
        if (!m.isOwner)
          Padding(
            padding: const EdgeInsets.only(top: G1.s2),
            child: Text(s.t('payout_account_owner_only'), style: text.bodySmall),
          ),
      ],
      const SizedBox(height: G1.s6),
      Text(s.t('payments_received').toUpperCase(), style: text.labelSmall),
      const SizedBox(height: G1.s2),
      if (_payments.isEmpty)
        Text(s.t('payments_none'), style: text.bodySmall)
      else
        card([
          for (final p in _payments)
            ListTile(
              leading: CircleAvatar(
                backgroundColor: c.surface2,
                child: Text(p.payerName.characters.first, style: text.titleSmall),
              ),
              title: Text(p.payerName),
              subtitle: Text(formatWhen(context, p.at)),
              trailing: Text('+${p.amount.format()}',
                  style: text.titleSmall?.copyWith(color: c.success, fontFeatures: G1.tabular)),
            ),
        ]),
    ];
  }

  String _payoutLine(S s, MerchantSummary? sum) {
    final n = sum?.next;
    if (n == null) {
      return sum?.pending == null || sum!.pending!.isZero ? s.t('payout_nothing') : s.t('payout_collecting');
    }
    return switch (n.status) {
      'on_hold' => s.t('payout_hold_${n.holdReason ?? 'review'}'),
      'sent' => s.t('payout_sent'),
      _ => s.t('payout_waiting'),
    };
  }
}

/// Picks a photo with the camera or from the gallery. Tests replace it.
typedef PhotoPicker = Future<List<int>?> Function(BuildContext context);

Future<List<int>?> pickDocumentPhoto(BuildContext context) async {
  final s = S.of(context);
  final source = await _sheet<ImageSource>(
    context,
    SafeArea(
      child: Column(mainAxisSize: MainAxisSize.min, children: [
        ListTile(
          leading: const Icon(Icons.photo_camera_outlined),
          title: Text(s.t('doc_take_photo')),
          onTap: () => Navigator.pop(context, ImageSource.camera),
        ),
        ListTile(
          leading: const Icon(Icons.photo_library_outlined),
          title: Text(s.t('doc_choose_photo')),
          onTap: () => Navigator.pop(context, ImageSource.gallery),
        ),
        const SizedBox(height: G1.s3),
      ]),
    ),
  );
  if (source == null) return null;
  // Large enough to read every line of a certificate, small enough to send over a slow connection.
  final file = await ImagePicker().pickImage(source: source, maxWidth: 2400, maxHeight: 2400, imageQuality: 85);
  return file?.readAsBytes();
}

/// The documents an application needs, with what happened to each, while it's under review.
class DocumentsCard extends StatefulWidget {
  const DocumentsCard({super.key, required this.merchant, this.pick = pickDocumentPhoto});
  final Merchant merchant;
  final PhotoPicker pick;

  @override
  State<DocumentsCard> createState() => _DocumentsCardState();
}

class _DocumentsCardState extends State<DocumentsCard> {
  MerchantDocuments? _docs;
  ApiProblem? _problem;
  String? _sending; // the kind being sent

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    try {
      final d = await AppScope.read(context).api.documents(widget.merchant.id);
      if (mounted) {
        setState(() {
          _docs = d;
          _problem = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  Future<void> _send(String kind) async {
    final s = S.of(context);
    final messenger = ScaffoldMessenger.of(context);
    final api = AppScope.read(context).api;
    final bytes = await widget.pick(context);
    if (bytes == null || !mounted) return;
    setState(() => _sending = kind);
    try {
      await api.uploadDocument(widget.merchant.id, kind, bytes);
      messenger.showSnackBar(SnackBar(content: Text(s.t('doc_sent'))));
      await _load();
    } on ApiProblem catch (p) {
      messenger.showSnackBar(SnackBar(content: Text(p.message.isNotEmpty ? p.message : s.t('error_generic'))));
    } finally {
      if (mounted) setState(() => _sending = null);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final docs = _docs;
    final owner = widget.merchant.isOwner;
    return Container(
      decoration: BoxDecoration(
          color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
      padding: const EdgeInsets.symmetric(vertical: G1.s3),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: G1.s4),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(s.t('docs_title'), style: text.titleMedium),
            const SizedBox(height: G1.s1),
            Text(owner ? s.t('docs_intro') : s.t('docs_owner_only'), style: text.bodySmall),
          ]),
        ),
        if (_problem != null)
          Padding(
            padding: const EdgeInsets.all(G1.s4),
            child: ProblemBanner(_problem!, onRetry: _load),
          )
        else if (docs == null)
          const Padding(padding: EdgeInsets.all(G1.s4), child: Center(child: CircularProgressIndicator()))
        else
          for (final kind in docs.required) _row(context, kind, docs.latest(kind), owner),
      ]),
    );
  }

  Widget _row(BuildContext context, String kind, MerchantDocument? d, bool owner) {
    final s = S.of(context);
    final c = G1.c(context);
    final (icon, color, status) = switch (d?.status) {
      'accepted' => (Icons.check_circle, c.success, s.t('doc_accepted')),
      'rejected' => (Icons.error, c.danger, s.t('doc_rejected', {'reason': d!.reviewNote ?? ''})),
      'submitted' => (Icons.schedule, c.brand, s.t('doc_submitted')),
      _ => (Icons.radio_button_unchecked, c.inkMuted, s.t('doc_missing')),
    };
    final Widget? action = !owner || d?.status == 'accepted'
        ? null
        : _sending == kind
            ? const SizedBox(width: 24, height: 24, child: CircularProgressIndicator(strokeWidth: 2.5))
            : TextButton(
                onPressed: _sending == null ? () => _send(kind) : null,
                child: Text(d == null ? s.t('doc_add') : s.t('doc_replace')),
              );
    return ListTile(
      leading: Icon(icon, color: color),
      title: Text(s.t('doc_$kind')),
      subtitle: Text(status),
      trailing: action,
    );
  }
}

class _QrSheet extends StatelessWidget {
  const _QrSheet({required this.name, required this.payload, required this.merchantNo});
  final String name, payload, merchantNo;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(G1.s5, 0, G1.s5, G1.s5),
        child: Column(mainAxisSize: MainAxisSize.min, children: [
          Text(s.t('qr_scan_to_pay', {'shop': name}), style: text.titleLarge, textAlign: TextAlign.center),
          const SizedBox(height: G1.s1),
          Text(s.t('qr_works_with'), style: text.bodySmall, textAlign: TextAlign.center),
          const SizedBox(height: G1.s4),
          Container(
            padding: const EdgeInsets.all(G1.s4),
            // Always dark on white, in dark mode too: scanners need the contrast.
            decoration: BoxDecoration(color: Colors.white, borderRadius: BorderRadius.circular(G1.rL)),
            child: Semantics(
              label: s.t('qr_semantic', {'shop': name}),
              child: QrImageView(data: payload, size: 240, backgroundColor: Colors.white),
            ),
          ),
          const SizedBox(height: G1.s3),
          Text('${s.t('merchant_no')} ${formatMerchantNo(merchantNo)}',
              style: text.titleMedium?.copyWith(fontFeatures: G1.tabular)),
          const SizedBox(height: G1.s4),
          SizedBox(
            width: double.infinity,
            child: PrimaryButton(label: s.t('done'), onPressed: () => Navigator.of(context).pop()),
          ),
        ]),
      ),
    );
  }
}

/// Where payouts go. Owners only; the PIN confirms it. Payouts to a new account start 48 hours later.
class _PayoutAccountSheet extends StatefulWidget {
  const _PayoutAccountSheet({required this.merchantId});
  final String merchantId;

  @override
  State<_PayoutAccountSheet> createState() => _PayoutAccountSheetState();
}

class _PayoutAccountSheetState extends State<_PayoutAccountSheet> {
  String _rail = 'telebirr';
  final _account = TextEditingController(), _bank = TextEditingController();
  bool _askPin = false, _busy = false;
  String? _fieldError, _pinError;

  bool get _bankRail => _rail == 'ethswitch';

  @override
  void dispose() {
    _account.dispose();
    _bank.dispose();
    super.dispose();
  }

  String? _accountValue() {
    if (_bankRail) {
      final a = _account.text.replaceAll(' ', '');
      return RegExp(r'^\d{8,20}$').hasMatch(a) && RegExp(r'^[A-Za-z0-9]{2,11}$').hasMatch(_bank.text.trim()) ? a : null;
    }
    return normalizePhone(_account.text);
  }

  void _continue() {
    final s = S.of(context);
    if (_accountValue() == null) {
      setState(() => _fieldError = s.t(_bankRail ? 'payout_bank_invalid' : 'payout_wallet_invalid'));
      return;
    }
    setState(() {
      _fieldError = null;
      _askPin = true;
    });
  }

  Future<void> _save(String pin) async {
    final api = AppScope.read(context).api;
    setState(() {
      _busy = true;
      _pinError = null;
    });
    try {
      final saved = await api.setPayoutAccount(widget.merchantId,
          rail: _rail, bankCode: _bankRail ? _bank.text.trim() : '', account: _accountValue()!, pin: pin);
      if (mounted) Navigator.of(context).pop(saved);
    } on ApiProblem catch (p) {
      if (!mounted) return;
      if (p.code == 'validation_failed') {
        setState(() {
          _askPin = false;
          _fieldError = p.message;
        });
      } else {
        setState(() => _pinError = p.message.isNotEmpty ? p.message : S.of(context).t('error_generic'));
      }
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    if (_askPin) {
      return SafeArea(
        child: Padding(
          padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            Text(s.t('payout_pin_title'), style: text.titleLarge, textAlign: TextAlign.center),
            const SizedBox(height: G1.s1),
            Text(s.t('payout_safety'), style: text.bodySmall, textAlign: TextAlign.center),
            const SizedBox(height: G1.s6),
            PinPad(onComplete: _save, error: _pinError, busy: _busy),
          ]),
        ),
      );
    }
    return Padding(
      padding: EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4 + MediaQuery.of(context).viewInsets.bottom),
      child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        Text(s.t('payout_account'), style: text.titleLarge),
        const SizedBox(height: G1.s4),
        Wrap(spacing: G1.s2, runSpacing: G1.s2, children: [
          for (final (rail, label) in [
            ('telebirr', 'telebirr'),
            ('cbe_birr', 'CBE Birr'),
            ('mpesa_et', 'M-PESA'),
            ('ethswitch', s.t('payout_bank')),
          ])
            ChoiceChip(
              label: Text(label),
              selected: _rail == rail,
              onSelected: (_) => setState(() {
                _rail = rail;
                _fieldError = null;
              }),
            ),
        ]),
        const SizedBox(height: G1.s4),
        if (_bankRail) ...[
          TextField(
            controller: _bank,
            textCapitalization: TextCapitalization.characters,
            decoration: InputDecoration(labelText: s.t('payout_bank_code'), helperText: s.t('payout_bank_code_help')),
          ),
          const SizedBox(height: G1.s3),
        ],
        TextField(
          controller: _account,
          keyboardType: _bankRail ? TextInputType.number : TextInputType.phone,
          decoration: InputDecoration(
            labelText: s.t(_bankRail ? 'payout_account_number' : 'payout_wallet_number'),
            errorText: _fieldError,
            errorMaxLines: 3,
          ),
        ),
        const SizedBox(height: G1.s3),
        Text(s.t('payout_safety'), style: text.bodySmall),
        const SizedBox(height: G1.s4),
        PrimaryButton(label: s.t('continue'), onPressed: _continue),
      ]),
    );
  }
}

class PayoutsScreen extends StatefulWidget {
  const PayoutsScreen(this.merchant, {super.key});
  final Merchant merchant;

  @override
  State<PayoutsScreen> createState() => _PayoutsScreenState();
}

class _PayoutsScreenState extends State<PayoutsScreen> {
  List<Payout>? _list;
  ApiProblem? _problem;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    try {
      final list = await AppScope.read(context).api.payouts(widget.merchant.id);
      if (mounted) {
        setState(() {
          _list = list;
          _problem = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final list = _list;
    Widget row(String label, String value, {bool strong = false}) => Padding(
          padding: const EdgeInsets.symmetric(vertical: 3),
          child: Row(children: [
            Expanded(child: Text(label, style: strong ? text.titleSmall : text.bodyMedium)),
            Text(value, style: (strong ? text.titleSmall : text.bodyMedium)?.copyWith(fontFeatures: G1.tabular)),
          ]),
        );
    return Scaffold(
      appBar: AppBar(title: Text(s.t('payout_history'))),
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
            if (_problem != null) ProblemBanner(_problem!, onRetry: _load),
            if (list == null && _problem == null) const Center(child: CircularProgressIndicator()),
            if (list != null && list.isEmpty) Text(s.t('payouts_none'), style: text.bodyMedium),
            for (final p in list ?? const <Payout>[])
              Container(
                margin: const EdgeInsets.only(bottom: G1.s3),
                padding: const EdgeInsets.all(G1.s4),
                decoration: BoxDecoration(
                    color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  Row(children: [
                    Expanded(
                        child: Text(s.t('payout_period', {'from': _day(p.periodStart), 'to': _day(p.periodEnd)}),
                            style: text.bodySmall)),
                    _PayoutPill(p.status),
                  ]),
                  const SizedBox(height: G1.s2),
                  row(s.t('payout_gross'), p.gross.format()),
                  row(s.t('payout_commission'), '−${p.commission.format()}'),
                  const Divider(),
                  row(s.t('payout_net'), p.net.format(), strong: true),
                  if (p.destination.isNotEmpty) Text(p.destination, style: text.bodySmall),
                  if (p.status == 'on_hold') ...[
                    const SizedBox(height: G1.s2),
                    Text(s.t('payout_hold_${p.holdReason ?? 'review'}'),
                        style: text.bodySmall?.copyWith(color: c.warning)),
                  ],
                  if (p.status == 'scheduled') ...[
                    const SizedBox(height: G1.s2),
                    Text(s.t('payout_waiting'), style: text.bodySmall),
                  ],
                ]),
              ),
          ]),
        ),
      ),
    );
  }
}

class _PayoutPill extends StatelessWidget {
  const _PayoutPill(this.status);
  final String status;

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final (bg, fg) = switch (status) {
      'settled' => (c.successTint, c.success),
      'on_hold' => (c.warningTint, c.warning),
      _ => (c.brandTint, c.brand),
    };
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: G1.s2, vertical: 1),
      decoration: BoxDecoration(color: bg, borderRadius: BorderRadius.circular(G1.rPill)),
      child: Text(S.of(context).t('payout_status_$status'),
          style: Theme.of(context).textTheme.labelSmall?.copyWith(color: fg, letterSpacing: 0)),
    );
  }
}
