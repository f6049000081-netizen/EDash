import 'api.dart';
import 'money.dart';

/// A business the signed-in person belongs to, with their role there.
class Merchant {
  Merchant({
    required this.id,
    required this.tradeName,
    required this.status,
    required this.role,
    required this.categoryCode,
    required this.city,
    this.merchantNo,
  });

  final String id;
  final String tradeName;
  final String status; // kyb_review, active, suspended …
  final String role; // owner, manager, cashier, accountant
  final String categoryCode;
  final String city;
  final String? merchantNo;

  bool get active => status == 'active';
  bool get seesMoney => role != 'cashier';
  bool get isOwner => role == 'owner';

  factory Merchant.fromJson(Map<String, dynamic> j) => Merchant(
        id: j['id'] as String,
        tradeName: j['trade_name'] as String,
        status: j['status'] as String,
        role: j['role'] as String? ?? '',
        categoryCode: j['category_code'] as String? ?? '',
        city: j['city'] as String? ?? '',
        merchantNo: j['merchant_no'] as String?,
      );
}

/// "00427781" → "0042 7781", as printed under the QR.
String formatMerchantNo(String n) => n.length == 8 ? '${n.substring(0, 4)} ${n.substring(4)}' : n;

class NextPayout {
  NextPayout(this.net, this.status, this.holdReason, this.scheduledFor);
  final Money net;
  final String status;
  final String? holdReason;
  final DateTime scheduledFor;
}

class MerchantSummary {
  MerchantSummary(this.salesToday, this.paymentsToday, this.averageBasket, this.pending, this.next);
  final Money salesToday;
  final int paymentsToday;
  final Money averageBasket;
  final Money? pending; // null for cashiers
  final NextPayout? next;

  factory MerchantSummary.fromJson(Map<String, dynamic> j) {
    final n = j['next_payout'] as Map<String, dynamic>?;
    return MerchantSummary(
      Money.fromJson(j['sales_today'] as Map<String, dynamic>),
      (j['payments_today'] as num).toInt(),
      Money.fromJson(j['average_basket'] as Map<String, dynamic>),
      j['pending_payout'] == null ? null : Money.fromJson(j['pending_payout'] as Map<String, dynamic>),
      n == null
          ? null
          : NextPayout(Money.fromJson(n['net'] as Map<String, dynamic>), n['status'] as String,
              n['hold_reason'] as String?, DateTime.parse(n['scheduled_for'] as String).toLocal()),
    );
  }
}

class ShopPayment {
  ShopPayment(this.id, this.payerName, this.amount, this.at);
  final String id;
  final String payerName;
  final Money amount;
  final DateTime at;

  factory ShopPayment.fromJson(Map<String, dynamic> j) => ShopPayment(j['id'] as String, j['payer_name'] as String,
      Money.fromJson(j['amount'] as Map<String, dynamic>), DateTime.parse(j['completed_at'] as String).toLocal());
}

class Payout {
  Payout({
    required this.id,
    required this.gross,
    required this.commission,
    required this.net,
    required this.status,
    required this.periodStart,
    required this.periodEnd,
    required this.destination,
    this.holdReason,
    this.settledAt,
  });

  final String id;
  final Money gross, commission, net;
  final String status; // scheduled, sent, settled, on_hold
  final String? holdReason;
  final DateTime periodStart, periodEnd;
  final String destination;
  final DateTime? settledAt;

  factory Payout.fromJson(Map<String, dynamic> j) => Payout(
        id: j['id'] as String,
        gross: Money.fromJson(j['gross'] as Map<String, dynamic>),
        commission: Money.fromJson(j['commission'] as Map<String, dynamic>),
        net: Money.fromJson(j['net'] as Map<String, dynamic>),
        status: j['status'] as String,
        holdReason: j['hold_reason'] as String?,
        periodStart: DateTime.parse(j['period_start'] as String).toLocal(),
        periodEnd: DateTime.parse(j['period_end'] as String).toLocal(),
        destination: j['destination_label'] as String? ?? '',
        settledAt: j['settled_at'] == null ? null : DateTime.parse(j['settled_at'] as String).toLocal(),
      );
}

class PayoutAccount {
  PayoutAccount(this.rail, this.label, this.payoutsFrom);
  final String rail;
  final String label;
  final DateTime payoutsFrom;

  factory PayoutAccount.fromJson(Map<String, dynamic> j) =>
      PayoutAccount(j['rail'] as String, j['label'] as String, DateTime.parse(j['payouts_from'] as String).toLocal());
}

/// A document sent so the business can be verified (KYB).
class MerchantDocument {
  MerchantDocument(
      {required this.id, required this.kind, required this.status, required this.uploadedAt, this.reviewNote});
  final String id;
  final String kind; // tin_certificate, trade_licence, owner_id, other
  final String status; // submitted, accepted, rejected
  final DateTime uploadedAt;
  final String? reviewNote; // why it was rejected

  factory MerchantDocument.fromJson(Map<String, dynamic> j) => MerchantDocument(
        id: j['id'] as String,
        kind: j['kind'] as String,
        status: j['status'] as String,
        uploadedAt: DateTime.parse(j['uploaded_at'] as String).toLocal(),
        reviewNote: j['review_note'] as String?,
      );
}

/// The documents sent, and the kinds the application needs.
class MerchantDocuments {
  MerchantDocuments(this.items, this.required);
  final List<MerchantDocument> items;
  final List<String> required;

  /// The latest document of a kind, if one was sent.
  MerchantDocument? latest(String kind) {
    MerchantDocument? best;
    for (final d in items) {
      if (d.kind == kind && (best == null || d.uploadedAt.isAfter(best.uploadedAt))) best = d;
    }
    return best;
  }
}

/// The type of an image file, from its first bytes (the server checks the same way).
String imageType(List<int> b) =>
    (b.length > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF) ? 'image/jpeg' : 'image/png';

/// Category codes a business can apply under (the server checks the same list).
const merchantCategories = ['5411', '5812', '5814', '5912', '5651', '5732', '5251', '7230', '4121', '8299', '5999'];

/// A browser asking to sign in to the web business console, as the app shows it before approval.
class WebLogin {
  WebLogin(this.browser, this.ip, this.createdAt);
  final String browser;
  final String ip;
  final DateTime createdAt;

  factory WebLogin.fromJson(Map<String, dynamic> j) =>
      WebLogin(j['browser'] as String, j['ip'] as String? ?? '', DateTime.parse(j['created_at'] as String).toLocal());
}

/// A browser signed in to the web business console.
class WebSession {
  WebSession(this.id, this.shopName, this.browser, this.ip, this.lastSeenAt);
  final String id;
  final String shopName;
  final String browser;
  final String ip;
  final DateTime lastSeenAt;

  factory WebSession.fromJson(Map<String, dynamic> j) => WebSession(j['id'] as String, j['shop_name'] as String,
      j['browser'] as String, j['ip'] as String? ?? '', DateTime.parse(j['last_seen_at'] as String).toLocal());
}

extension BusinessApi on GebeyaApi {
  Future<WebLogin> lookupWebLogin(String code) async =>
      WebLogin.fromJson(await api.post('/v1/web-logins/lookup', {'code': code}));

  Future<WebLogin> approveWebLogin(String merchantId, String code, String pin) async => WebLogin.fromJson(
      await api.post('/v1/web-logins/approve', {'code': code, 'merchant_id': merchantId, 'pin': pin}));

  Future<List<WebSession>> webSessions() async {
    final j = await api.get('/v1/me/web-sessions');
    return [for (final x in j['items'] as List) WebSession.fromJson(x as Map<String, dynamic>)];
  }

  Future<void> signOutWebSession(String id) async => api.delete('/v1/me/web-sessions/$id');

  Future<List<Merchant>> myMerchants() async {
    final j = await api.get('/v1/me/merchants');
    return [for (final m in j['items'] as List) Merchant.fromJson(m as Map<String, dynamic>)];
  }

  Future<Merchant> merchant(String id) async => Merchant.fromJson(await api.get('/v1/merchants/$id'));

  Future<Merchant> applyMerchant({
    required String legalName,
    required String tradeName,
    required String tin,
    String licence = '',
    required String category,
    required String city,
  }) async =>
      Merchant.fromJson(await api.post('/v1/merchants', {
        'legal_name': legalName,
        'trade_name': tradeName,
        'tin': tin,
        if (licence.isNotEmpty) 'business_licence': licence,
        'category_code': category,
        'city': city,
      }));

  Future<MerchantSummary> merchantSummary(String id) async =>
      MerchantSummary.fromJson(await api.get('/v1/merchants/$id/summary'));

  Future<List<ShopPayment>> shopPayments(String id, {int limit = 20}) async {
    final j = await api.get('/v1/merchants/$id/payments', query: {'limit': '$limit'});
    return [for (final p in j['items'] as List) ShopPayment.fromJson(p as Map<String, dynamic>)];
  }

  Future<({String payload, String merchantNo})> shopQr(String id) async {
    final j = await api.get('/v1/merchants/$id/qr');
    return (payload: j['payload'] as String, merchantNo: j['merchant_no'] as String);
  }

  Future<List<Payout>> payouts(String id, {int limit = 30}) async {
    final j = await api.get('/v1/merchants/$id/payouts', query: {'limit': '$limit'});
    return [for (final p in j['items'] as List) Payout.fromJson(p as Map<String, dynamic>)];
  }

  /// The current payout account, or null when none is set.
  Future<PayoutAccount?> payoutAccount(String id) async {
    try {
      return PayoutAccount.fromJson(await api.get('/v1/merchants/$id/payout-destination'));
    } on ApiProblem catch (p) {
      if (p.code == 'not_found') return null;
      rethrow;
    }
  }

  Future<MerchantDocuments> documents(String id) async {
    final j = await api.get('/v1/merchants/$id/documents');
    return MerchantDocuments(
      [for (final d in j['items'] as List) MerchantDocument.fromJson(d as Map<String, dynamic>)],
      [for (final k in j['required'] as List) k as String],
    );
  }

  Future<MerchantDocument> uploadDocument(String id, String kind, List<int> bytes) async => MerchantDocument.fromJson(
      await api.postBytes('/v1/merchants/$id/documents', bytes, imageType(bytes), query: {'kind': kind}));

  Future<PayoutAccount> setPayoutAccount(String id,
          {required String rail, String bankCode = '', required String account, required String pin}) async =>
      PayoutAccount.fromJson(await api.put('/v1/merchants/$id/payout-destination', {
        'rail': rail,
        if (bankCode.isNotEmpty) 'bank_code': bankCode,
        'account': account,
        'pin': pin,
      }));
}
