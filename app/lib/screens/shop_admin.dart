import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/business.dart';
import '../core/commerce.dart';
import '../core/money.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';

String _problemText(BuildContext context, ApiProblem p) =>
    p.message.isNotEmpty ? p.message : S.of(context).t('error_generic');

Future<T?> _sheet<T>(BuildContext context, Widget child) => showModalBottomSheet<T>(
      context: context,
      isScrollControlled: true,
      showDragHandle: true,
      shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(G1.rXl))),
      builder: (_) => Padding(padding: EdgeInsets.only(bottom: MediaQuery.of(context).viewInsets.bottom), child: child),
    );

bool _canManage(Merchant m) => m.role == 'owner' || m.role == 'manager';
bool _canHandle(Merchant m) => m.role != 'accountant';

/// The shop's orders: in progress (next to handle on top) and finished. Polls while open, so a cashier
/// sees new orders arrive.
class ShopOrdersScreen extends StatefulWidget {
  const ShopOrdersScreen(this.merchant, {super.key});
  final Merchant merchant;

  @override
  State<ShopOrdersScreen> createState() => _ShopOrdersScreenState();
}

class _ShopOrdersScreenState extends State<ShopOrdersScreen> {
  bool _active = true;
  List<Order>? _orders;
  ApiProblem? _problem;
  Timer? _poll;
  final _busy = <String>{};

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
    _poll = Timer.periodic(const Duration(seconds: 15), (_) {
      if (_active) _load();
    });
  }

  @override
  void dispose() {
    _poll?.cancel();
    super.dispose();
  }

  Future<void> _load() async {
    final active = _active;
    try {
      final list = await AppScope.read(context).api.shopOrders(widget.merchant.id, active: active);
      if (mounted && active == _active) {
        setState(() {
          _orders = list;
          _problem = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  Future<void> _act(Order o, String action, {String? code, String? note}) async {
    setState(() => _busy.add(o.id));
    try {
      await AppScope.read(context).api.orderAction(widget.merchant.id, o.id, action, code: code, note: note);
      if (mounted && action == 'hand_over') {
        ScaffoldMessenger.of(context).showSnackBar(
            SnackBar(content: Text(S.of(context).t('handed_over', {'number': o.number, 'amount': o.total.format()}))));
      }
      await _load();
    } on ApiProblem catch (p) {
      if (mounted) ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(_problemText(context, p))));
      if (p.code == 'conflict') _load();
    } finally {
      if (mounted) setState(() => _busy.remove(o.id));
    }
  }

  Future<void> _handOver(Order o) async {
    final code = await _sheet<String>(context, _CodeSheet(order: o));
    if (code != null) await _act(o, 'hand_over', code: code);
  }

  Future<void> _cancel(Order o) async {
    final note = await _sheet<String>(context, _CancelSheet(order: o));
    if (note != null) await _act(o, 'cancel', note: note);
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    final list = _orders;
    return Scaffold(
      appBar: AppBar(title: Text(s.t('shop_orders'))),
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
            SegmentedButton<bool>(
              segments: [
                ButtonSegment(value: true, label: Text(s.t('orders_active'))),
                ButtonSegment(value: false, label: Text(s.t('orders_done'))),
              ],
              selected: {_active},
              onSelectionChanged: (v) {
                setState(() {
                  _active = v.first;
                  _orders = null;
                });
                _load();
              },
            ),
            const SizedBox(height: G1.s4),
            if (_problem != null) ProblemBanner(_problem!, onRetry: _load),
            if (list == null && _problem == null) const Center(child: CircularProgressIndicator()),
            if (list != null && list.isEmpty)
              Text(s.t(_active ? 'shop_orders_none' : 'shop_orders_done_none'), style: text.bodyMedium),
            if (list != null)
              for (final o in list) ...[
                _OrderCard(
                  order: o,
                  canHandle: _canHandle(widget.merchant),
                  busy: _busy.contains(o.id),
                  onStep: (a) => _act(o, a),
                  onHandOver: () => _handOver(o),
                  onCancel: () => _cancel(o),
                ),
                const SizedBox(height: G1.s3),
              ],
          ]),
        ),
      ),
    );
  }
}

class _OrderCard extends StatelessWidget {
  const _OrderCard({
    required this.order,
    required this.canHandle,
    required this.busy,
    required this.onStep,
    required this.onHandOver,
    required this.onCancel,
  });
  final Order order;
  final bool canHandle;
  final bool busy;
  final void Function(String action) onStep;
  final VoidCallback onHandOver;
  final VoidCallback onCancel;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final o = order;
    final compact = FilledButton.styleFrom(minimumSize: const Size(0, 44));
    String hm(DateTime t) => '${t.hour.toString().padLeft(2, '0')}:${t.minute.toString().padLeft(2, '0')}';
    final (String? next, String? nextLabel) = switch (o.status) {
      'placed' => ('accept', s.t('order_accept')),
      'accepted' => ('ready', s.t(o.delivery ? 'order_packed' : 'order_ready')),
      'ready' when o.delivery => ('on_the_way', s.t('order_send_out')),
      _ => (null, null),
    };
    return Container(
      padding: const EdgeInsets.all(G1.s4),
      decoration: BoxDecoration(
          color: c.surface1,
          borderRadius: BorderRadius.circular(G1.rL),
          border: Border.all(color: o.status == 'placed' ? c.accent : c.line, width: o.status == 'placed' ? 2 : 1)),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Row(children: [
          Expanded(child: Text('${o.number} · ${s.t(o.delivery ? 'delivery' : 'pickup')}', style: text.titleSmall)),
          AmountText(o.total, style: text.titleMedium),
        ]),
        const SizedBox(height: G1.s1),
        Text(
            [
              s.t('order_status_${o.status}'),
              if (o.placedAt != null) formatWhen(context, o.placedAt!),
              if (o.status == 'placed' && o.acceptBy != null) s.t('accept_by', {'time': hm(o.acceptBy!)}),
            ].join(' · '),
            style: text.bodySmall?.copyWith(color: o.status == 'placed' ? c.warning : null)),
        const SizedBox(height: G1.s3),
        for (final i in o.items) Text('${i.quantity} × ${i.title}', style: text.bodyMedium),
        if (o.note.isNotEmpty) ...[
          const SizedBox(height: G1.s2),
          Text(s.t('customer_note', {'note': o.note}), style: text.bodyMedium?.copyWith(fontStyle: FontStyle.italic)),
        ],
        const SizedBox(height: G1.s2),
        Text(
            [
              if ((o.customerName ?? '').isNotEmpty) o.customerName!,
              if (o.customerPhone != null) o.customerPhone!,
            ].join(' · '),
            style: text.bodySmall),
        if (o.delivery && o.deliveryAddress.isNotEmpty) Text(o.deliveryAddress, style: text.bodySmall),
        if (o.status == 'cancelled')
          Text(s.t('order_cancelled_${o.cancelReason}', {'shop': o.shopName}), style: text.bodySmall),
        if (canHandle && o.inProgress) ...[
          const SizedBox(height: G1.s3),
          Wrap(spacing: G1.s2, runSpacing: G1.s2, children: [
            if (next != null)
              FilledButton(onPressed: busy ? null : () => onStep(next), style: compact, child: Text(nextLabel!)),
            if (o.status != 'placed')
              FilledButton.tonal(
                  onPressed: busy ? null : onHandOver, style: compact, child: Text(s.t('order_hand_over'))),
            TextButton(
              onPressed: busy ? null : onCancel,
              style: TextButton.styleFrom(foregroundColor: c.danger, minimumSize: const Size(0, 44)),
              child: Text(s.t(o.status == 'placed' ? 'order_decline' : 'order_cancel_shop')),
            ),
          ]),
        ],
      ]),
    );
  }
}

class _CodeSheet extends StatefulWidget {
  const _CodeSheet({required this.order});
  final Order order;

  @override
  State<_CodeSheet> createState() => _CodeSheetState();
}

class _CodeSheetState extends State<_CodeSheet> {
  final _code = TextEditingController();

  @override
  void dispose() {
    _code.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
        child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
          Text(s.t('handover_title', {'number': widget.order.number}), style: text.titleLarge),
          const SizedBox(height: G1.s2),
          Text(s.t('handover_ask', {'amount': widget.order.total.format()}), style: text.bodyMedium),
          const SizedBox(height: G1.s4),
          TextField(
            controller: _code,
            autofocus: true,
            keyboardType: TextInputType.number,
            maxLength: 4,
            textAlign: TextAlign.center,
            style: text.displaySmall?.copyWith(letterSpacing: 12),
            inputFormatters: [FilteringTextInputFormatter.digitsOnly],
            decoration: InputDecoration(labelText: s.t('handover_code')),
            onChanged: (_) => setState(() {}),
          ),
          const SizedBox(height: G1.s3),
          PrimaryButton(
            label: s.t('order_hand_over'),
            onPressed: _code.text.length == 4 ? () => Navigator.of(context).pop(_code.text) : null,
          ),
        ]),
      ),
    );
  }
}

class _CancelSheet extends StatefulWidget {
  const _CancelSheet({required this.order});
  final Order order;

  @override
  State<_CancelSheet> createState() => _CancelSheetState();
}

class _CancelSheetState extends State<_CancelSheet> {
  final _note = TextEditingController();

  @override
  void dispose() {
    _note.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
        child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
          Text(
              s.t(widget.order.status == 'placed' ? 'order_decline_q' : 'order_cancel_shop_q',
                  {'number': widget.order.number}),
              style: text.titleLarge),
          const SizedBox(height: G1.s2),
          Text(s.t('order_cancel_shop_body', {'amount': widget.order.total.format()}), style: text.bodyMedium),
          const SizedBox(height: G1.s4),
          TextField(
            controller: _note,
            autofocus: true,
            maxLength: 280,
            maxLines: 3,
            decoration:
                InputDecoration(labelText: s.t('order_cancel_shop_note'), hintText: s.t('order_cancel_shop_hint')),
            onChanged: (_) => setState(() {}),
          ),
          const SizedBox(height: G1.s3),
          FilledButton(
            style: FilledButton.styleFrom(backgroundColor: c.danger, foregroundColor: c.onBrand),
            onPressed: _note.text.trim().isEmpty ? null : () => Navigator.of(context).pop(_note.text.trim()),
            child: Text(s.t(widget.order.status == 'placed' ? 'order_decline' : 'order_cancel_shop')),
          ),
        ]),
      ),
    );
  }
}

/// The shop's catalog and ordering settings.
class CatalogScreen extends StatefulWidget {
  const CatalogScreen(this.merchant, {super.key});
  final Merchant merchant;

  @override
  State<CatalogScreen> createState() => _CatalogScreenState();
}

class _CatalogScreenState extends State<CatalogScreen> {
  Shop? _shop;
  ApiProblem? _problem;
  bool _saving = false;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    try {
      final shop = await AppScope.read(context).api.shopSettings(widget.merchant.id);
      if (mounted) {
        setState(() {
          _shop = shop;
          _problem = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  Future<void> _setOpen(bool open) async {
    final sh = _shop!;
    setState(() => _saving = true);
    try {
      final saved = await AppScope.read(context).api.saveShop(sh.merchantId,
          open: open,
          pickup: sh.pickup,
          delivery: sh.delivery,
          deliveryFee: sh.deliveryFee,
          deliveryArea: sh.deliveryArea,
          about: sh.about);
      if (mounted) setState(() => _shop = saved);
    } on ApiProblem catch (p) {
      if (mounted) ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(_problemText(context, p))));
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  Future<void> _settings() async {
    final saved = await _sheet<Shop>(context, _SettingsSheet(shop: _shop!));
    if (saved != null && mounted) setState(() => _shop = saved);
  }

  Future<void> _product([Product? p]) async {
    final saved = await _sheet<Product>(context, _ProductSheet(merchantId: widget.merchant.id, product: p));
    if (saved != null) _load();
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final sh = _shop;
    final manage = _canManage(widget.merchant);
    final box =
        BoxDecoration(color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line));
    return Scaffold(
      appBar: AppBar(title: Text(s.t('catalog'))),
      floatingActionButton: manage && sh != null
          ? FloatingActionButton.extended(
              onPressed: () => _product(), icon: const Icon(Icons.add), label: Text(s.t('product_add')))
          : null,
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.fromLTRB(G1.s4, G1.s4, G1.s4, 96), children: [
            if (_problem != null) ProblemBanner(_problem!, onRetry: _load),
            if (sh == null && _problem == null) const Center(child: CircularProgressIndicator()),
            if (sh != null) ...[
              Container(
                decoration: box,
                clipBehavior: Clip.antiAlias,
                child: Column(children: [
                  SwitchListTile(
                    title: Text(s.t('shop_open')),
                    subtitle: Text(s.t(sh.open ? 'shop_open_on' : 'shop_open_off')),
                    value: sh.open,
                    onChanged: manage && !_saving ? _setOpen : null,
                  ),
                  const Divider(indent: G1.s4),
                  ListTile(
                    title: Text(s.t('shop_how')),
                    subtitle: Text([
                      if (sh.pickup) s.t('pickup'),
                      if (sh.delivery)
                        '${s.t('delivery')} (${sh.deliveryFee.isZero ? s.t('free') : sh.deliveryFee.format()})',
                    ].join(' · ')),
                    trailing: manage ? const Icon(Icons.chevron_right) : null,
                    onTap: manage ? _settings : null,
                  ),
                ]),
              ),
              const SizedBox(height: G1.s5),
              Text(s.t('products').toUpperCase(), style: text.labelSmall),
              const SizedBox(height: G1.s2),
              if (sh.products.isEmpty)
                Text(s.t(manage ? 'products_none_manage' : 'products_none'), style: text.bodyMedium)
              else
                Container(
                  decoration: box,
                  clipBehavior: Clip.antiAlias,
                  child: Column(children: [
                    for (var i = 0; i < sh.products.length; i++) ...[
                      if (i > 0) const Divider(indent: G1.s4),
                      ListTile(
                        title: Text(sh.products[i].title),
                        subtitle: Text([
                          if (sh.products[i].category.isNotEmpty) sh.products[i].category,
                          if (sh.products[i].status == 'hidden') s.t('product_hidden'),
                          if (sh.products[i].stock != null)
                            sh.products[i].soldOut
                                ? s.t('sold_out')
                                : s.t('in_stock', {'n': '${sh.products[i].stock}'}),
                        ].join(' · ')),
                        trailing: AmountText(sh.products[i].price, style: text.titleSmall),
                        onTap: manage ? () => _product(sh.products[i]) : null,
                      ),
                    ],
                  ]),
                ),
            ],
          ]),
        ),
      ),
    );
  }
}

class _SettingsSheet extends StatefulWidget {
  const _SettingsSheet({required this.shop});
  final Shop shop;

  @override
  State<_SettingsSheet> createState() => _SettingsSheetState();
}

class _SettingsSheetState extends State<_SettingsSheet> {
  late bool _pickup = widget.shop.pickup, _delivery = widget.shop.delivery;
  late final _fee = TextEditingController(text: widget.shop.deliveryFee.isZero ? '' : widget.shop.deliveryFee.decimal);
  late final _area = TextEditingController(text: widget.shop.deliveryArea);
  late final _about = TextEditingController(text: widget.shop.about);
  bool _busy = false;
  String? _error;

  @override
  void dispose() {
    _fee.dispose();
    _area.dispose();
    _about.dispose();
    super.dispose();
  }

  Future<void> _save() async {
    final s = S.of(context);
    final fee = _fee.text.trim().isEmpty ? Money.zeroEtb : parseAmount(_fee.text);
    if (fee == null) {
      setState(() => _error = s.t('amount_invalid'));
      return;
    }
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final saved = await AppScope.read(context).api.saveShop(widget.shop.merchantId,
          open: widget.shop.open,
          pickup: _pickup,
          delivery: _delivery,
          deliveryFee: fee,
          deliveryArea: _area.text.trim(),
          about: _about.text.trim());
      if (mounted) Navigator.of(context).pop(saved);
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _error = _problemText(context, p));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    return SafeArea(
      child: SingleChildScrollView(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
        child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
          Text(s.t('shop_how'), style: text.titleLarge),
          SwitchListTile(
            contentPadding: EdgeInsets.zero,
            title: Text(s.t('pickup')),
            subtitle: Text(s.t('pickup_setting')),
            value: _pickup,
            onChanged: (v) => setState(() => _pickup = v),
          ),
          SwitchListTile(
            contentPadding: EdgeInsets.zero,
            title: Text(s.t('delivery')),
            subtitle: Text(s.t('delivery_setting')),
            value: _delivery,
            onChanged: (v) => setState(() => _delivery = v),
          ),
          if (_delivery) ...[
            TextField(
              controller: _fee,
              keyboardType: const TextInputType.numberWithOptions(decimal: true),
              decoration: InputDecoration(labelText: s.t('delivery_fee'), prefixText: 'ETB ', hintText: '0'),
            ),
            const SizedBox(height: G1.s2),
            TextField(
              controller: _area,
              maxLength: 200,
              decoration: InputDecoration(labelText: s.t('delivery_area'), hintText: s.t('delivery_area_hint')),
            ),
          ],
          TextField(
            controller: _about,
            maxLength: 500,
            maxLines: 3,
            decoration: InputDecoration(labelText: s.t('shop_about')),
          ),
          if (_error != null) Text(_error!, style: text.bodyMedium?.copyWith(color: c.danger)),
          const SizedBox(height: G1.s3),
          PrimaryButton(label: s.t('save'), busy: _busy, onPressed: _save),
        ]),
      ),
    );
  }
}

class _ProductSheet extends StatefulWidget {
  const _ProductSheet({required this.merchantId, this.product});
  final String merchantId;
  final Product? product;

  @override
  State<_ProductSheet> createState() => _ProductSheetState();
}

class _ProductSheetState extends State<_ProductSheet> {
  late final _title = TextEditingController(text: widget.product?.title ?? '');
  late final _description = TextEditingController(text: widget.product?.description ?? '');
  late final _category = TextEditingController(text: widget.product?.category ?? '');
  late final _price = TextEditingController(text: widget.product?.price.decimal ?? '');
  late final _stock = TextEditingController(text: widget.product?.stock?.toString() ?? '');
  late bool _hidden = widget.product?.status == 'hidden';
  bool _busy = false;
  String? _error;

  @override
  void dispose() {
    for (final c in [_title, _description, _category, _price, _stock]) {
      c.dispose();
    }
    super.dispose();
  }

  Future<void> _save({bool archive = false}) async {
    final s = S.of(context);
    final price = parseAmount(_price.text);
    final stock = _stock.text.trim().isEmpty ? null : int.tryParse(_stock.text.trim());
    if (_title.text.trim().isEmpty) {
      setState(() => _error = s.t('field_required'));
      return;
    }
    if (price == null || price.isZero) {
      setState(() => _error = s.t('amount_invalid'));
      return;
    }
    if (_stock.text.trim().isNotEmpty && stock == null) {
      setState(() => _error = s.t('stock_invalid'));
      return;
    }
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final saved = await AppScope.read(context).api.saveProduct(widget.merchantId,
          id: widget.product?.id,
          title: _title.text.trim(),
          description: _description.text.trim(),
          category: _category.text.trim(),
          price: price,
          stock: stock,
          status: archive ? 'archived' : (_hidden ? 'hidden' : 'active'));
      if (mounted) Navigator.of(context).pop(saved);
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _error = _problemText(context, p));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    return SafeArea(
      child: SingleChildScrollView(
        padding: const EdgeInsets.fromLTRB(G1.s4, 0, G1.s4, G1.s4),
        child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
          Text(s.t(widget.product == null ? 'product_add' : 'product_edit'), style: text.titleLarge),
          const SizedBox(height: G1.s3),
          TextField(controller: _title, maxLength: 80, decoration: InputDecoration(labelText: s.t('product_name'))),
          TextField(
            controller: _price,
            keyboardType: const TextInputType.numberWithOptions(decimal: true),
            decoration: InputDecoration(labelText: s.t('price'), prefixText: 'ETB '),
          ),
          const SizedBox(height: G1.s2),
          TextField(
              controller: _category,
              maxLength: 40,
              decoration: InputDecoration(labelText: s.t('shelf'), hintText: s.t('shelf_hint'))),
          TextField(
              controller: _description,
              maxLength: 500,
              maxLines: 2,
              decoration: InputDecoration(labelText: s.t('product_description'))),
          TextField(
            controller: _stock,
            keyboardType: TextInputType.number,
            inputFormatters: [FilteringTextInputFormatter.digitsOnly],
            decoration: InputDecoration(labelText: s.t('stock'), helperText: s.t('stock_help')),
          ),
          SwitchListTile(
            contentPadding: EdgeInsets.zero,
            title: Text(s.t('product_hide')),
            subtitle: Text(s.t('product_hide_help')),
            value: _hidden,
            onChanged: (v) => setState(() => _hidden = v),
          ),
          if (_error != null) Text(_error!, style: text.bodyMedium?.copyWith(color: c.danger)),
          const SizedBox(height: G1.s3),
          PrimaryButton(label: s.t('save'), busy: _busy, onPressed: _save),
          if (widget.product != null)
            TextButton(
              onPressed: _busy ? null : () => _save(archive: true),
              style: TextButton.styleFrom(foregroundColor: c.danger),
              child: Text(s.t('product_archive')),
            ),
        ]),
      ),
    );
  }
}
