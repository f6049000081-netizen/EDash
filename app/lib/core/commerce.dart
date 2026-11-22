import 'api.dart';
import 'money.dart';

/// A shop taking orders in the app.
class Shop {
  Shop({
    required this.merchantId,
    required this.name,
    required this.category,
    required this.city,
    required this.open,
    required this.pickup,
    required this.delivery,
    required this.deliveryFee,
    required this.deliveryArea,
    required this.about,
    required this.productCount,
    this.products = const [],
  });

  final String merchantId;
  final String name;
  final String category;
  final String city;
  final bool open;
  final bool pickup;
  final bool delivery;
  final Money deliveryFee;
  final String deliveryArea;
  final String about;
  final int productCount;
  final List<Product> products;

  factory Shop.fromJson(Map<String, dynamic> j) => Shop(
        merchantId: j['merchant_id'] as String,
        name: j['name'] as String,
        category: j['category'] as String? ?? '',
        city: j['city'] as String? ?? '',
        open: j['open'] as bool? ?? false,
        pickup: j['pickup'] as bool? ?? true,
        delivery: j['delivery'] as bool? ?? false,
        deliveryFee: Money.fromJson(j['delivery_fee'] as Map<String, dynamic>),
        deliveryArea: j['delivery_area'] as String? ?? '',
        about: j['about'] as String? ?? '',
        productCount: (j['product_count'] as num?)?.toInt() ?? 0,
        products: [for (final p in (j['products'] as List?) ?? const []) Product.fromJson(p as Map<String, dynamic>)],
      );

  /// Products by the shop's own shelf names, in the order the server sent them.
  Map<String, List<Product>> get shelves {
    final out = <String, List<Product>>{};
    for (final p in products) {
      out.putIfAbsent(p.category, () => []).add(p);
    }
    return out;
  }
}

class Product {
  Product({
    required this.id,
    required this.title,
    required this.description,
    required this.category,
    required this.price,
    required this.stock,
    required this.soldOut,
    required this.status,
  });

  final String id;
  final String title;
  final String description;
  final String category;
  final Money price;
  final int? stock; // null: the shop doesn't count it
  final bool soldOut;
  final String status; // active, hidden, archived

  factory Product.fromJson(Map<String, dynamic> j) => Product(
        id: j['id'] as String,
        title: j['title'] as String,
        description: j['description'] as String? ?? '',
        category: j['category'] as String? ?? '',
        price: Money.fromJson(j['price'] as Map<String, dynamic>),
        stock: (j['stock'] as num?)?.toInt(),
        soldOut: j['sold_out'] as bool? ?? false,
        status: j['status'] as String? ?? 'active',
      );

  /// The most a customer can put in the basket.
  int get maxQuantity => stock == null ? 99 : (stock! < 99 ? stock! : 99);
}

/// What the customer is about to order from one shop. Prices are the shop's current ones; the server
/// prices the order again when it's placed.
class Basket {
  Basket(this.shop);
  final Shop shop;
  final Map<String, int> _qty = {};

  int quantity(String productId) => _qty[productId] ?? 0;

  /// Sets a product's quantity within 0 and what the shop has. Returns the quantity kept.
  int set(Product p, int n) {
    final q = n.clamp(0, p.maxQuantity);
    if (q == 0) {
      _qty.remove(p.id);
    } else {
      _qty[p.id] = q;
    }
    return q;
  }

  int add(Product p) => set(p, quantity(p.id) + 1);
  int remove(Product p) => set(p, quantity(p.id) - 1);

  bool get isEmpty => _qty.isEmpty;
  int get count => _qty.values.fold(0, (a, b) => a + b);

  List<({Product product, int quantity})> get lines => [
        for (final p in shop.products)
          if (_qty.containsKey(p.id)) (product: p, quantity: _qty[p.id]!)
      ];

  Money get subtotal => Money(lines.fold(0, (a, l) => a + l.product.price.amountMinor * l.quantity), 'ETB');

  Money total(String channel) =>
      Money(subtotal.amountMinor + (channel == 'delivery' ? shop.deliveryFee.amountMinor : 0), 'ETB');

  List<Map<String, dynamic>> toJson() => [
        for (final l in lines) {'product_id': l.product.id, 'quantity': l.quantity}
      ];
}

class OrderItem {
  OrderItem(this.title, this.unitPrice, this.quantity, this.lineTotal);
  final String title;
  final Money unitPrice;
  final int quantity;
  final Money lineTotal;
}

class Order {
  Order({
    required this.id,
    required this.number,
    required this.status,
    required this.channel,
    required this.merchantId,
    required this.shopName,
    required this.items,
    required this.subtotal,
    required this.deliveryFee,
    required this.total,
    required this.deliveryAddress,
    required this.note,
    required this.createdAt,
    this.paymentIntentId,
    this.paymentStatus = '',
    this.handoverCode,
    this.cancelReason = '',
    this.cancelNote = '',
    this.customerName,
    this.customerPhone,
    this.placedAt,
    this.acceptBy,
    this.completedAt,
    this.overdue = false,
    this.inReview = false,
  });

  final String id;
  final String number;
  final String status; // pending_payment, placed, accepted, ready, on_the_way, delivered, cancelled
  final String channel; // pickup, delivery
  final String merchantId;
  final String shopName;
  final List<OrderItem> items;
  final Money subtotal;
  final Money deliveryFee;
  final Money total;
  final String deliveryAddress;
  final String note;
  final DateTime createdAt;
  final String? paymentIntentId;
  final String paymentStatus;
  final String? handoverCode; // the customer's own orders, while in progress
  final String cancelReason;
  final String cancelNote;
  final String? customerName; // the shop's view
  final String? customerPhone;
  final DateTime? placedAt;
  final DateTime? acceptBy;
  final bool overdue; // accepted a day ago and still not handed over
  final bool inReview; // still not handed over after three days: our team settles it
  final DateTime? completedAt;

  bool get delivery => channel == 'delivery';
  bool get inProgress => const {'placed', 'accepted', 'ready', 'on_the_way'}.contains(status);
  bool get done => status == 'delivered' || status == 'cancelled';

  /// The customer may still cancel: the shop hasn't started on it.
  bool get customerCanCancel =>
      status == 'placed' || (status == 'pending_payment' && paymentStatus != 'held_for_review');

  /// The customer may confirm they have it: the shop has accepted it.
  bool get customerCanConfirm => const {'accepted', 'ready', 'on_the_way'}.contains(status);

  /// The steps shown to the customer, and how many are done.
  List<String> get steps => delivery
      ? const ['placed', 'accepted', 'ready', 'on_the_way', 'delivered']
      : const ['placed', 'accepted', 'ready', 'delivered'];

  int get stepsDone {
    if (status == 'pending_payment' || status == 'cancelled') return 0;
    return steps.indexOf(status) + 1;
  }

  static DateTime? _t(Object? v) => v == null ? null : DateTime.parse(v as String).toLocal();

  factory Order.fromJson(Map<String, dynamic> j) {
    final customer = j['customer'] as Map<String, dynamic>?;
    return Order(
      id: j['id'] as String,
      number: j['number'] as String,
      status: j['status'] as String,
      channel: j['channel'] as String,
      merchantId: j['merchant_id'] as String,
      shopName: j['shop_name'] as String? ?? '',
      items: [
        for (final i in (j['items'] as List?) ?? const [])
          OrderItem(
            (i as Map<String, dynamic>)['title'] as String,
            Money.fromJson(i['unit_price'] as Map<String, dynamic>),
            (i['quantity'] as num).toInt(),
            Money.fromJson(i['line_total'] as Map<String, dynamic>),
          )
      ],
      subtotal: Money.fromJson(j['subtotal'] as Map<String, dynamic>),
      deliveryFee: Money.fromJson(j['delivery_fee'] as Map<String, dynamic>),
      total: Money.fromJson(j['total'] as Map<String, dynamic>),
      deliveryAddress: j['delivery_address'] as String? ?? '',
      note: j['note'] as String? ?? '',
      createdAt: _t(j['created_at'])!,
      paymentIntentId: j['payment_intent_id'] as String?,
      paymentStatus: j['payment_status'] as String? ?? '',
      handoverCode: j['handover_code'] as String?,
      cancelReason: j['cancel_reason'] as String? ?? '',
      cancelNote: j['cancel_note'] as String? ?? '',
      customerName: customer?['name'] as String?,
      customerPhone: customer?['phone'] as String?,
      placedAt: _t(j['placed_at']),
      acceptBy: _t(j['accept_by']),
      completedAt: _t(j['completed_at']),
      overdue: j['overdue'] as bool? ?? false,
      inReview: j['in_review'] as bool? ?? false,
    );
  }

  /// "2 × Injera, Berbere" for lists.
  String get summary => items.map((i) => i.quantity > 1 ? '${i.quantity} × ${i.title}' : i.title).join(', ');
}

extension CommerceApi on GebeyaApi {
  Future<List<Shop>> shops() async {
    final j = await api.get('/v1/shops');
    return [for (final s in j['items'] as List) Shop.fromJson(s as Map<String, dynamic>)];
  }

  Future<Shop> shop(String merchantId) async => Shop.fromJson(await api.get('/v1/shops/$merchantId'));

  /// Creates the order and its payment. Pass the same [key] when retrying the same checkout.
  Future<Order> placeOrder(Basket basket,
          {required String channel, String address = '', String note = '', required String key}) async =>
      Order.fromJson(await api.post(
          '/v1/orders',
          {
            'merchant_id': basket.shop.merchantId,
            'channel': channel,
            'items': basket.toJson(),
            if (channel == 'delivery') 'delivery_address': address,
            if (note.isNotEmpty) 'note': note,
          },
          idempotencyKey: key));

  Future<List<Order>> orders() async {
    final j = await api.get('/v1/orders');
    return [for (final o in j['items'] as List) Order.fromJson(o as Map<String, dynamic>)];
  }

  Future<Order> order(String id) async => Order.fromJson(await api.get('/v1/orders/$id'));

  Future<Order> cancelOrder(String id) async => Order.fromJson(await api.post('/v1/orders/$id/cancel', {}));

  Future<Order> orderReceived(String id) async => Order.fromJson(await api.post('/v1/orders/$id/received', {}));

  // The shop's side.

  Future<Shop> shopSettings(String merchantId) async => Shop.fromJson(await api.get('/v1/merchants/$merchantId/shop'));

  Future<Shop> saveShop(String merchantId,
          {required bool open,
          required bool pickup,
          required bool delivery,
          required Money deliveryFee,
          String deliveryArea = '',
          String about = ''}) async =>
      Shop.fromJson(await api.put('/v1/merchants/$merchantId/shop', {
        'open': open,
        'pickup': pickup,
        'delivery': delivery,
        'delivery_fee': deliveryFee.toJson(),
        'delivery_area': deliveryArea,
        'about': about,
      }));

  Future<Product> saveProduct(String merchantId,
      {String? id,
      required String title,
      String description = '',
      String category = '',
      required Money price,
      int? stock,
      String status = 'active'}) async {
    final body = {
      'title': title,
      'description': description,
      'category': category,
      'price': price.toJson(),
      'stock': stock,
      'status': status,
    };
    return Product.fromJson(id == null
        ? await api.post('/v1/merchants/$merchantId/products', body)
        : await api.put('/v1/merchants/$merchantId/products/$id', body));
  }

  Future<List<Order>> shopOrders(String merchantId, {bool active = true}) async {
    final j = await api.get('/v1/merchants/$merchantId/orders', query: {'state': active ? 'active' : 'done'});
    return [for (final o in j['items'] as List) Order.fromJson(o as Map<String, dynamic>)];
  }

  /// accept, ready, on_the_way, hand_over (with [code]) or cancel (with [note]).
  Future<Order> orderAction(String merchantId, String orderId, String action, {String? code, String? note}) async =>
      Order.fromJson(await api.post('/v1/merchants/$merchantId/orders/$orderId/$action', {
        if (code != null) 'code': code,
        if (note != null) 'note': note,
      }));
}
