import 'dart:async';

import 'package:flutter/material.dart';

import '../core/api.dart';
import '../core/app_state.dart';
import '../core/commerce.dart';
import '../core/money.dart';
import '../l10n/strings.dart';
import '../ui/theme.dart';
import '../ui/widgets.dart';
import 'pay.dart';

String _problemText(BuildContext context, ApiProblem p) =>
    p.message.isNotEmpty ? p.message : S.of(context).t('error_generic');

void _snack(BuildContext context, String text) =>
    ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(text)));

/// Shops taking orders, for the Explore tab.
class ShopList extends StatefulWidget {
  const ShopList({super.key});

  @override
  State<ShopList> createState() => ShopListState();
}

class ShopListState extends State<ShopList> {
  List<Shop>? _shops;
  ApiProblem? _problem;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => load());
  }

  Future<void> load() async {
    try {
      final list = await AppScope.read(context).api.shops();
      if (mounted) {
        setState(() {
          _shops = list;
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
    if (_problem != null && _shops == null) return ProblemBanner(_problem!, onRetry: load);
    if (_shops == null) {
      return const Padding(padding: EdgeInsets.all(G1.s6), child: Center(child: CircularProgressIndicator()));
    }
    if (_shops!.isEmpty) {
      return Container(
        padding: const EdgeInsets.all(G1.s5),
        decoration: BoxDecoration(color: c.brandTint, borderRadius: BorderRadius.circular(G1.rL)),
        child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Icon(Icons.storefront_outlined, color: c.brand),
          const SizedBox(width: G1.s3),
          Expanded(child: Text(s.t('shops_none'), style: text.bodyMedium)),
        ]),
      );
    }
    return Container(
      decoration: BoxDecoration(
          color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
      clipBehavior: Clip.antiAlias,
      child: Column(children: [
        for (var i = 0; i < _shops!.length; i++) ...[
          if (i > 0) const Divider(indent: 72),
          _ShopRow(_shops![i]),
        ],
      ]),
    );
  }
}

class _ShopRow extends StatelessWidget {
  const _ShopRow(this.shop);
  final Shop shop;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final ways = [if (shop.pickup) s.t('pickup'), if (shop.delivery) s.t('delivery')].join(' · ');
    return ListTile(
      contentPadding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s1),
      leading: CircleAvatar(
        radius: 22,
        backgroundColor: c.accentTint,
        child: Text(shop.name.characters.first.toUpperCase(), style: text.titleSmall?.copyWith(color: c.onAccent)),
      ),
      title: Text(shop.name, maxLines: 1, overflow: TextOverflow.ellipsis),
      subtitle: Text(shop.open ? ways : s.t('shop_closed_now'),
          style: text.bodySmall?.copyWith(color: shop.open ? null : c.warning)),
      trailing: const Icon(Icons.chevron_right),
      onTap: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => ShopScreen(shop.merchantId))),
    );
  }
}

/// A shop's products, with the basket.
class ShopScreen extends StatefulWidget {
  const ShopScreen(this.merchantId, {super.key});
  final String merchantId;

  @override
  State<ShopScreen> createState() => _ShopScreenState();
}

class _ShopScreenState extends State<ShopScreen> {
  Shop? _shop;
  Basket? _basket;
  ApiProblem? _problem;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    try {
      final shop = await AppScope.read(context).api.shop(widget.merchantId);
      if (!mounted) return;
      setState(() {
        // Keep what's in the basket across a refresh, within what's still on sale.
        final old = _basket;
        _shop = shop;
        _basket = Basket(shop);
        if (old != null) {
          for (final p in shop.products) {
            if (!p.soldOut) _basket!.set(p, old.quantity(p.id));
          }
        }
        _problem = null;
      });
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  Future<void> _checkout() async {
    final ordered =
        await Navigator.of(context).push<bool>(MaterialPageRoute(builder: (_) => CheckoutScreen(basket: _basket!)));
    if (ordered == true) {
      if (mounted) setState(() => _basket = Basket(_shop!));
    } else {
      _load(); // prices or stock may have changed
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final shop = _shop;
    final basket = _basket;
    return Scaffold(
      appBar: AppBar(title: Text(shop?.name ?? '')),
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.fromLTRB(G1.s4, G1.s2, G1.s4, 120), children: [
            if (_problem != null) ProblemBanner(_problem!, onRetry: _load),
            if (shop == null && _problem == null) const Center(child: CircularProgressIndicator()),
            if (shop != null) ...[
              Container(
                padding: const EdgeInsets.all(G1.s4),
                decoration: BoxDecoration(
                    color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  if (shop.about.isNotEmpty) ...[
                    Text(shop.about, style: text.bodyMedium),
                    const SizedBox(height: G1.s2)
                  ],
                  if (!shop.open)
                    Text(s.t('shop_closed_now'), style: text.titleSmall?.copyWith(color: c.warning))
                  else ...[
                    if (shop.pickup) _fact(context, Icons.storefront_outlined, s.t('pickup_at', {'city': shop.city})),
                    if (shop.delivery)
                      _fact(
                          context,
                          Icons.delivery_dining_outlined,
                          s.t('delivery_to', {
                            'area': shop.deliveryArea,
                            'fee': shop.deliveryFee.isZero ? s.t('free') : shop.deliveryFee.format()
                          })),
                  ],
                ]),
              ),
              for (final shelf in shop.shelves.entries) ...[
                const SizedBox(height: G1.s5),
                if (shelf.key.isNotEmpty) ...[
                  Text(shelf.key.toUpperCase(), style: text.labelSmall),
                  const SizedBox(height: G1.s2),
                ],
                Container(
                  decoration: BoxDecoration(
                      color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
                  clipBehavior: Clip.antiAlias,
                  child: Column(children: [
                    for (var i = 0; i < shelf.value.length; i++) ...[
                      if (i > 0) const Divider(indent: G1.s4),
                      _ProductRow(
                        product: shelf.value[i],
                        quantity: basket!.quantity(shelf.value[i].id),
                        enabled: shop.open,
                        onAdd: () => setState(() => basket.add(shelf.value[i])),
                        onRemove: () => setState(() => basket.remove(shelf.value[i])),
                      ),
                    ],
                  ]),
                ),
              ],
            ],
          ]),
        ),
      ),
      bottomNavigationBar: basket == null || basket.isEmpty
          ? null
          : SafeArea(
              child: Padding(
                padding: const EdgeInsets.fromLTRB(G1.s4, G1.s2, G1.s4, G1.s3),
                child: PrimaryButton(
                  label: s.t('basket_go', {'n': '${basket.count}', 'amount': basket.subtotal.format()}),
                  onPressed: _checkout,
                ),
              ),
            ),
    );
  }

  Widget _fact(BuildContext context, IconData icon, String line) => Padding(
        padding: const EdgeInsets.only(top: G1.s1),
        child: Row(children: [
          Icon(icon, size: 18, color: G1.c(context).inkMuted),
          const SizedBox(width: G1.s2),
          Expanded(child: Text(line, style: Theme.of(context).textTheme.bodySmall)),
        ]),
      );
}

class _ProductRow extends StatelessWidget {
  const _ProductRow({
    required this.product,
    required this.quantity,
    required this.enabled,
    required this.onAdd,
    required this.onRemove,
  });
  final Product product;
  final int quantity;
  final bool enabled;
  final VoidCallback onAdd;
  final VoidCallback onRemove;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final p = product;
    return Padding(
      padding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s3),
      child: Row(children: [
        if (p.thumbUrl != null) ...[
          ExcludeSemantics(child: ProductThumb(p)),
          const SizedBox(width: G1.s3),
        ],
        Expanded(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(p.title, style: text.titleSmall),
            if (p.description.isNotEmpty)
              Text(p.description, style: text.bodySmall, maxLines: 2, overflow: TextOverflow.ellipsis),
            const SizedBox(height: G1.s1),
            AmountText(p.price, style: text.bodyMedium?.copyWith(color: c.inkMuted)),
          ]),
        ),
        if (p.soldOut)
          Text(s.t('sold_out'), style: text.labelSmall?.copyWith(color: c.inkSubtle))
        else if (quantity == 0)
          OutlinedButton(
              onPressed: enabled ? onAdd : null,
              style: OutlinedButton.styleFrom(minimumSize: const Size(72, 40)),
              child: Text(s.t('add')))
        else
          QuantityStepper(
            quantity: quantity,
            name: p.title,
            onAdd: quantity < p.maxQuantity ? onAdd : null,
            onRemove: onRemove,
          ),
      ]),
    );
  }
}

/// A product's photo from the API server, square with rounded corners; a plain tile when there's none
/// or it can't load. Photos are public shop content, so no session is sent.
class ProductThumb extends StatelessWidget {
  const ProductThumb(this.product, {super.key, this.size = 56, this.full = false});
  final Product product;
  final double size;
  final bool full; // the large photo rather than the thumbnail

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final path = full ? product.photoUrl : product.thumbUrl;
    final empty = Container(
      width: size,
      height: size,
      color: c.surface2,
      child: Icon(Icons.image_outlined, color: c.inkSubtle, size: size * 0.4),
    );
    return ClipRRect(
      borderRadius: BorderRadius.circular(G1.rS + 2),
      child: path == null
          ? empty
          : Image.network(
              AppScope.read(context).api.api.baseUrl + path,
              width: size,
              height: size,
              fit: BoxFit.cover,
              semanticLabel: product.title,
              errorBuilder: (_, __, ___) => empty,
            ),
    );
  }
}

/// − 2 + with names for screen readers.
class QuantityStepper extends StatelessWidget {
  const QuantityStepper({super.key, required this.quantity, required this.name, this.onAdd, this.onRemove});
  final int quantity;
  final String name;
  final VoidCallback? onAdd;
  final VoidCallback? onRemove;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    Widget round(IconData icon, String label, VoidCallback? onTap) => Semantics(
          button: true,
          label: label,
          excludeSemantics: true,
          child: IconButton.filledTonal(
            onPressed: onTap,
            icon: Icon(icon, size: 18),
            style: IconButton.styleFrom(backgroundColor: c.surface2, minimumSize: const Size(40, 40)),
          ),
        );
    return Row(mainAxisSize: MainAxisSize.min, children: [
      round(Icons.remove, s.t('qty_less', {'name': name}), onRemove),
      SizedBox(
        width: 32,
        child:
            Text('$quantity', textAlign: TextAlign.center, style: text.titleMedium?.copyWith(fontFeatures: G1.tabular)),
      ),
      round(Icons.add, s.t('qty_more', {'name': name}), onAdd),
    ]);
  }
}

/// A delivery time's day in Addis Ababa: Today, Tomorrow or dd/mm.
String slotDay(BuildContext context, DateTime start) {
  final s = S.of(context);
  final t = toEat(start);
  final now = toEat(DateTime.now());
  final day = DateTime.utc(t.year, t.month, t.day);
  final today = DateTime.utc(now.year, now.month, now.day);
  final diff = day.difference(today).inDays;
  if (diff == 0) return s.t('today');
  if (diff == 1) return s.t('tomorrow');
  return '${t.day.toString().padLeft(2, '0')}/${t.month.toString().padLeft(2, '0')}';
}

/// "14:00–16:00" in Addis Ababa time.
String slotHours(DateTime start, DateTime end) {
  String hm(DateTime x) {
    final t = toEat(x);
    return '${t.hour.toString().padLeft(2, '0')}:${t.minute.toString().padLeft(2, '0')}';
  }

  final e = hm(end);
  return '${hm(start)}–${e == '00:00' ? '24:00' : e}';
}

/// "Tomorrow, 14:00–16:00".
String slotWhen(BuildContext context, DateTime start, DateTime end) =>
    '${slotDay(context, start)}, ${slotHours(start, end)}';

/// The basket, pickup or delivery, and paying.
class CheckoutScreen extends StatefulWidget {
  const CheckoutScreen({super.key, required this.basket});
  final Basket basket;

  @override
  State<CheckoutScreen> createState() => _CheckoutScreenState();
}

class _CheckoutScreenState extends State<CheckoutScreen> {
  late String _channel = widget.basket.shop.pickup ? 'pickup' : 'delivery';
  final _address = TextEditingController();
  final _note = TextEditingController();
  bool _busy = false;
  String? _error;
  // One key per checkout: a retry after a dropped connection doesn't place a second order.
  String _key = newIdempotencyKey();
  List<DeliverySlot>? _slots; // loaded when delivery is chosen at a shop with delivery times
  DeliverySlot? _slot;
  bool _slotsFailed = false;

  bool get _needsSlot => _channel == 'delivery' && widget.basket.shop.deliverySlots != null;

  @override
  void initState() {
    super.initState();
    if (_needsSlot) WidgetsBinding.instance.addPostFrameCallback((_) => _loadSlots());
  }

  Future<void> _loadSlots() async {
    setState(() => _slotsFailed = false);
    try {
      final list = await AppScope.read(context).api.deliverySlots(widget.basket.shop.merchantId);
      if (!mounted) return;
      setState(() {
        _slots = list;
        if (_slot != null && !list.any((x) => x.start == _slot!.start && !x.full)) _slot = null;
      });
    } on ApiProblem {
      if (mounted) setState(() => _slotsFailed = true);
    }
  }

  @override
  void dispose() {
    _address.dispose();
    _note.dispose();
    super.dispose();
  }

  Future<void> _pay() async {
    final s = S.of(context);
    if (_channel == 'delivery' && _address.text.trim().isEmpty) {
      setState(() => _error = s.t('address_needed'));
      return;
    }
    if (_needsSlot && _slot == null) {
      setState(() => _error = s.t('slot_needed'));
      return;
    }
    setState(() {
      _busy = true;
      _error = null;
    });
    final app = AppScope.read(context);
    try {
      final order = await app.api.placeOrder(widget.basket,
          channel: _channel,
          address: _address.text.trim(),
          note: _note.text.trim(),
          slotStart: _needsSlot ? _slot?.start : null,
          key: _key);
      final intent = await app.api.intent(order.paymentIntentId!);
      if (!mounted) return;
      final paid = await confirmPayment(context, intent);
      if (!mounted) return;
      if (paid == null) {
        // Backed out of the PIN: let the order go, so its stock is free again. A new tap starts afresh.
        app.api.cancelOrder(order.id).catchError((_) => order);
        _key = newIdempotencyKey();
        return;
      }
      app.refresh();
      if (paid.status == 'failed') {
        _key = newIdempotencyKey();
        await Navigator.of(context).push(MaterialPageRoute(builder: (_) => ResultScreen(intent: paid)));
        return;
      }
      Navigator.of(context).pop(true);
      Navigator.of(context).push(MaterialPageRoute(builder: (_) => OrderScreen(order.id)));
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _error = _problemText(context, p));
      if (p.code != 'network') _key = newIdempotencyKey();
      if (p.status == 409 && _needsSlot && mounted) _loadSlots(); // the time may have filled up
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Widget _slotPicker(BuildContext context) {
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    final c = G1.c(context);
    final list = _slots;
    if (_slotsFailed) {
      return Row(children: [
        Expanded(child: Text(s.t('slots_failed'), style: text.bodyMedium?.copyWith(color: c.danger))),
        TextButton(onPressed: _loadSlots, child: Text(s.t('retry'))),
      ]);
    }
    if (list == null) return const Padding(padding: EdgeInsets.all(G1.s3), child: LinearProgressIndicator());
    if (!list.any((x) => !x.full)) return Text(s.t('slots_none'), style: text.bodyMedium?.copyWith(color: c.danger));
    final days = <String, List<DeliverySlot>>{};
    for (final x in list) {
      days.putIfAbsent(slotDay(context, x.start), () => []).add(x);
    }
    return Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
      Text(s.t('slot_choose'), style: text.titleSmall),
      Text(s.t('slot_time_zone'), style: text.bodySmall),
      for (final d in days.entries) ...[
        const SizedBox(height: G1.s2),
        Text(d.key, style: text.labelLarge),
        const SizedBox(height: G1.s1),
        Wrap(spacing: G1.s2, runSpacing: G1.s2, children: [
          for (final x in d.value)
            ChoiceChip(
              label: Text(x.full ? '${slotHours(x.start, x.end)} · ${s.t('slot_full')}' : slotHours(x.start, x.end)),
              selected: _slot?.start == x.start,
              onSelected: x.full ? null : (_) => setState(() => _slot = x),
            ),
        ]),
      ],
    ]);
  }

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final b = widget.basket;
    final shop = b.shop;
    Widget line(String label, Money m, {bool strong = false}) => Padding(
          padding: const EdgeInsets.symmetric(vertical: G1.s1),
          child: Row(children: [
            Expanded(
                child: Text(label, style: strong ? text.titleSmall : text.bodyMedium?.copyWith(color: c.inkMuted))),
            AmountText(m, style: strong ? text.titleMedium : text.bodyMedium),
          ]),
        );
    final box =
        BoxDecoration(color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line));
    return Scaffold(
      appBar: AppBar(title: Text(s.t('checkout_title'))),
      body: SafeArea(
        child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
          Text(shop.name, style: text.titleLarge),
          const SizedBox(height: G1.s3),
          Container(
            decoration: box,
            clipBehavior: Clip.antiAlias,
            child: Column(children: [
              for (final l in b.lines)
                Padding(
                  padding: const EdgeInsets.fromLTRB(G1.s4, G1.s2, G1.s2, G1.s2),
                  child: Row(children: [
                    Expanded(
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        Text(l.product.title, style: text.titleSmall),
                        AmountText(Money(l.product.price.amountMinor * l.quantity, 'ETB'), style: text.bodySmall),
                      ]),
                    ),
                    QuantityStepper(
                      quantity: l.quantity,
                      name: l.product.title,
                      onAdd: l.quantity < l.product.maxQuantity ? () => setState(() => b.add(l.product)) : null,
                      onRemove: () => setState(() {
                        b.remove(l.product);
                        if (b.isEmpty) Navigator.of(context).pop(false);
                      }),
                    ),
                  ]),
                ),
            ]),
          ),
          const SizedBox(height: G1.s5),
          if (shop.pickup && shop.delivery)
            SegmentedButton<String>(
              segments: [
                ButtonSegment(value: 'pickup', label: Text(s.t('pickup')), icon: const Icon(Icons.storefront_outlined)),
                ButtonSegment(
                    value: 'delivery', label: Text(s.t('delivery')), icon: const Icon(Icons.delivery_dining_outlined)),
              ],
              selected: {_channel},
              onSelectionChanged: (v) {
                setState(() => _channel = v.first);
                if (_needsSlot && _slots == null) _loadSlots();
              },
            )
          else
            Text(s.t(_channel == 'pickup' ? 'pickup_only' : 'delivery_only'), style: text.bodyMedium),
          const SizedBox(height: G1.s3),
          if (_channel == 'delivery') ...[
            Text(s.t('delivery_to',
                {'area': shop.deliveryArea, 'fee': shop.deliveryFee.isZero ? s.t('free') : shop.deliveryFee.format()})),
            const SizedBox(height: G1.s2),
            TextField(
              controller: _address,
              maxLength: 300,
              maxLines: 2,
              decoration: InputDecoration(labelText: s.t('address_label'), hintText: s.t('address_hint')),
            ),
            if (_needsSlot) ...[_slotPicker(context), const SizedBox(height: G1.s3)],
          ] else
            Text(s.t('pickup_explain'), style: text.bodySmall),
          const SizedBox(height: G1.s2),
          TextField(
            controller: _note,
            maxLength: 280,
            decoration: InputDecoration(labelText: s.t('order_note_label')),
          ),
          const SizedBox(height: G1.s3),
          Container(
            decoration: box,
            padding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s2),
            child: Column(children: [
              line(s.t('items'), b.subtotal),
              if (_channel == 'delivery') line(s.t('delivery_fee'), shop.deliveryFee),
              const Divider(),
              line(s.t('total'), b.total(_channel), strong: true),
            ]),
          ),
          const SizedBox(height: G1.s2),
          Text(s.t('escrow_explain'), style: text.bodySmall),
          if (_error != null) ...[
            const SizedBox(height: G1.s3),
            Text(_error!, style: text.bodyMedium?.copyWith(color: c.danger)),
          ],
          const SizedBox(height: G1.s5),
          PrimaryButton(
            label: s.t('pay_amount', {'amount': b.total(_channel).format()}),
            onPressed: _busy ? null : _pay,
            busy: _busy,
          ),
        ]),
      ),
    );
  }
}

/// One order: where it is, the handover code, and what the customer can still do.
class OrderScreen extends StatefulWidget {
  const OrderScreen(this.orderId, {super.key});
  final String orderId;

  @override
  State<OrderScreen> createState() => _OrderScreenState();
}

class _OrderScreenState extends State<OrderScreen> {
  Order? _o;
  ApiProblem? _problem;
  Timer? _poll;
  bool _busy = false;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
    _poll = Timer.periodic(const Duration(seconds: 15), (_) {
      if (_o == null || !_o!.done) _load();
    });
  }

  @override
  void dispose() {
    _poll?.cancel();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final o = await AppScope.read(context).api.order(widget.orderId);
      if (mounted) {
        setState(() {
          _o = o;
          _problem = null;
        });
      }
    } on ApiProblem catch (p) {
      if (mounted) setState(() => _problem = p);
    }
  }

  Future<void> _act(Future<Order> Function() f) async {
    setState(() => _busy = true);
    try {
      final o = await f();
      if (mounted) setState(() => _o = o);
      if (mounted) AppScope.read(context).refresh();
    } on ApiProblem catch (p) {
      if (mounted) _snack(context, _problemText(context, p));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<bool> _ask(String title, String body, String yes) async =>
      await showDialog<bool>(
        context: context,
        builder: (ctx) => AlertDialog(
          title: Text(title),
          content: Text(body),
          actions: [
            TextButton(onPressed: () => Navigator.pop(ctx, false), child: Text(S.of(ctx).t('cancel'))),
            FilledButton(onPressed: () => Navigator.pop(ctx, true), child: Text(yes)),
          ],
        ),
      ) ??
      false;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final o = _o;
    final api = AppScope.read(context).api;
    final box =
        BoxDecoration(color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line));
    return Scaffold(
      appBar: AppBar(title: Text(o == null ? s.t('order') : '${s.t('order')} ${o.number}')),
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
            if (_problem != null) ProblemBanner(_problem!, onRetry: _load),
            if (o == null && _problem == null) const Center(child: CircularProgressIndicator()),
            if (o != null) ...[
              Text(o.shopName, style: text.titleLarge),
              const SizedBox(height: G1.s1),
              Semantics(
                liveRegion: true,
                child: Text(_headline(s, o), style: text.bodyLarge?.copyWith(color: c.inkMuted)),
              ),
              if (o.inProgress && (o.inReview || o.overdue)) ...[
                const SizedBox(height: G1.s3),
                Container(
                  padding: const EdgeInsets.all(G1.s3),
                  decoration: BoxDecoration(color: c.warningTint, borderRadius: BorderRadius.circular(G1.rM)),
                  child: Text(s.t(o.inReview ? 'order_in_review' : 'order_overdue', {'shop': o.shopName}),
                      style: text.bodyMedium),
                ),
              ],
              if (o.stepsDone > 0) ...[
                const SizedBox(height: G1.s4),
                _Progress(order: o),
              ],
              if (o.handoverCode != null && o.inProgress) ...[
                const SizedBox(height: G1.s5),
                Container(
                  padding: const EdgeInsets.all(G1.s5),
                  decoration: BoxDecoration(color: c.brandStrong, borderRadius: BorderRadius.circular(G1.rL)),
                  child: Column(children: [
                    Text(s.t('handover_code').toUpperCase(),
                        style: text.labelSmall?.copyWith(color: c.onBrand.withOpacity(0.85))),
                    const SizedBox(height: G1.s2),
                    Semantics(
                      label: o.handoverCode!.split('').join(' '),
                      excludeSemantics: true,
                      child: Text(o.handoverCode!,
                          style: text.displayMedium
                              ?.copyWith(color: c.onBrand, letterSpacing: 12, fontFeatures: G1.tabular)),
                    ),
                    const SizedBox(height: G1.s2),
                    Text(s.t('handover_explain'),
                        textAlign: TextAlign.center,
                        style: text.bodySmall?.copyWith(color: c.onBrand.withOpacity(0.85))),
                  ]),
                ),
              ],
              if (o.cancelNote.isNotEmpty) ...[
                const SizedBox(height: G1.s4),
                Container(
                  padding: const EdgeInsets.all(G1.s4),
                  decoration: BoxDecoration(color: c.warningTint, borderRadius: BorderRadius.circular(G1.rL)),
                  child: Text(s.t('shop_said', {'note': o.cancelNote}), style: text.bodyMedium),
                ),
              ],
              const SizedBox(height: G1.s5),
              Container(
                decoration: box,
                padding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s2),
                child: Column(children: [
                  for (final i in o.items)
                    Padding(
                      padding: const EdgeInsets.symmetric(vertical: G1.s1),
                      child: Row(children: [
                        Expanded(child: Text('${i.quantity} × ${i.title}', style: text.bodyMedium)),
                        AmountText(i.lineTotal, style: text.bodyMedium),
                      ]),
                    ),
                  if (!o.deliveryFee.isZero)
                    Padding(
                      padding: const EdgeInsets.symmetric(vertical: G1.s1),
                      child: Row(children: [
                        Expanded(child: Text(s.t('delivery_fee'), style: text.bodyMedium)),
                        AmountText(o.deliveryFee, style: text.bodyMedium),
                      ]),
                    ),
                  const Divider(),
                  Padding(
                    padding: const EdgeInsets.symmetric(vertical: G1.s1),
                    child: Row(children: [
                      Expanded(child: Text(s.t('total'), style: text.titleSmall)),
                      AmountText(o.total, style: text.titleMedium),
                    ]),
                  ),
                ]),
              ),
              const SizedBox(height: G1.s3),
              Text(
                  [
                    s.t(o.delivery ? 'delivery' : 'pickup'),
                    if (o.delivery) o.deliveryAddress,
                    if (o.slotStart != null) s.t('slot_arrives', {'when': slotWhen(context, o.slotStart!, o.slotEnd!)}),
                    formatWhen(context, o.createdAt),
                  ].join(' · '),
                  style: text.bodySmall),
              if (o.note.isNotEmpty) Text(s.t('your_note', {'note': o.note}), style: text.bodySmall),
              const SizedBox(height: G1.s6),
              if (o.customerCanConfirm)
                PrimaryButton(
                  label: s.t('order_received'),
                  busy: _busy,
                  onPressed: _busy
                      ? null
                      : () async {
                          if (await _ask(s.t('order_received_q'), s.t('order_received_body', {'shop': o.shopName}),
                              s.t('order_received_yes'))) {
                            await _act(() => api.orderReceived(o.id));
                          }
                        },
                ),
              if (o.customerCanCancel)
                TextButton(
                  onPressed: _busy
                      ? null
                      : () async {
                          if (await _ask(s.t('order_cancel_q'), s.t('order_cancel_body'), s.t('order_cancel_yes'))) {
                            await _act(() => api.cancelOrder(o.id));
                          }
                        },
                  child: Text(s.t('order_cancel')),
                ),
            ],
          ]),
        ),
      ),
    );
  }

  String _headline(S s, Order o) {
    if (o.status == 'cancelled') return s.t('order_cancelled_${o.cancelReason}', {'shop': o.shopName});
    if (o.status == 'pending_payment') {
      return s.t(o.paymentStatus == 'held_for_review' ? 'order_payment_checked' : 'order_not_paid');
    }
    if (o.status == 'placed' && o.acceptBy != null) {
      final h = o.acceptBy!.hour.toString().padLeft(2, '0'), m = o.acceptBy!.minute.toString().padLeft(2, '0');
      return s.t('order_head_placed', {'shop': o.shopName, 'time': '$h:$m'});
    }
    return s.t('order_head_${o.status}_${o.channel}');
  }
}

class _Progress extends StatelessWidget {
  const _Progress({required this.order});
  final Order order;

  @override
  Widget build(BuildContext context) {
    final s = S.of(context);
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    final steps = order.steps;
    final done = order.stepsDone;
    return Semantics(
      label: s.t('order_step_of', {'n': '$done', 'total': '${steps.length}'}),
      child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        Row(children: [
          for (var i = 0; i < steps.length; i++) ...[
            if (i > 0) const SizedBox(width: G1.s1),
            Expanded(
              child: Container(
                height: 4,
                decoration:
                    BoxDecoration(color: i < done ? c.brand : c.surface2, borderRadius: BorderRadius.circular(4)),
              ),
            ),
          ],
        ]),
        const SizedBox(height: G1.s2),
        Row(children: [
          for (var i = 0; i < steps.length; i++)
            Expanded(
              child: Text(s.t('step_${steps[i]}_${order.channel}'),
                  style: (i == done - 1 ? text.labelSmall?.copyWith(color: c.ink) : text.bodySmall)
                      ?.copyWith(fontSize: 11)),
            ),
        ]),
      ]),
    );
  }
}

/// The customer's orders.
class MyOrdersScreen extends StatefulWidget {
  const MyOrdersScreen({super.key});

  @override
  State<MyOrdersScreen> createState() => _MyOrdersScreenState();
}

class _MyOrdersScreenState extends State<MyOrdersScreen> {
  List<Order>? _orders;
  ApiProblem? _problem;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => _load());
  }

  Future<void> _load() async {
    try {
      final list = await AppScope.read(context).api.orders();
      if (mounted) {
        setState(() {
          _orders = list;
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
    final list = _orders;
    return Scaffold(
      appBar: AppBar(title: Text(s.t('my_orders'))),
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.all(G1.s4), children: [
            if (_problem != null) ProblemBanner(_problem!, onRetry: _load),
            if (list == null && _problem == null) const Center(child: CircularProgressIndicator()),
            if (list != null && list.isEmpty) Text(s.t('orders_none'), style: text.bodyMedium),
            if (list != null && list.isNotEmpty)
              Container(
                decoration: BoxDecoration(
                    color: c.surface1, borderRadius: BorderRadius.circular(G1.rL), border: Border.all(color: c.line)),
                clipBehavior: Clip.antiAlias,
                child: Column(children: [
                  for (var i = 0; i < list.length; i++) ...[
                    if (i > 0) const Divider(indent: G1.s4),
                    ListTile(
                      title: Text(list[i].shopName),
                      subtitle:
                          Text('${s.t('order_status_${list[i].status}')} · ${formatWhen(context, list[i].createdAt)}'),
                      trailing: AmountText(list[i].total, style: text.titleSmall),
                      onTap: () async {
                        await Navigator.of(context).push(MaterialPageRoute(builder: (_) => OrderScreen(list[i].id)));
                        _load();
                      },
                    ),
                  ],
                ]),
              ),
          ]),
        ),
      ),
    );
  }
}
