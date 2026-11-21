import 'dart:convert';

import 'package:crypto/crypto.dart';
import 'package:flutter/services.dart';

import 'api.dart' show newIdempotencyKey;

/// Why a biometric confirmation didn't produce a signature.
enum BiometricOutcome { cancelled, lockedOut, notEnrolled, unavailable }

class BiometricException implements Exception {
  BiometricException(this.outcome);
  final BiometricOutcome outcome;
  @override
  String toString() => 'BiometricException($outcome)';
}

/// Keys held in the phone's secure hardware (see MainActivity.kt):
/// - the DPoP key, which signs a proof for every API request so a copied token is useless elsewhere;
/// - the biometric key, which signs payment confirmations after the phone's fingerprint or face check.
///
/// Platforms without the native side (iOS until it's written, and tests) report no keys; the app then
/// uses plain bearer tokens and the PIN, as before.
abstract class DeviceKeys {
  /// The DPoP public key as base64url x and y, creating the key if needed. Null: no hardware keys.
  Future<({String x, String y})?> dpopPublicKey();

  /// ECDSA P-256/SHA-256 signature, ASN.1 DER.
  Future<Uint8List> dpopSign(Uint8List data);

  /// Forget the DPoP key, so the next sign-in binds a fresh one.
  Future<void> dpopReset();

  Future<bool> biometricAvailable();
  Future<bool> biometricEnrolled();

  /// Creates a new biometric-gated key and returns its public key (base64 DER SPKI).
  Future<String> biometricCreate();
  Future<void> biometricDelete();

  /// Shows the system fingerprint/face prompt and signs [data]. Returns a base64 DER signature or
  /// throws [BiometricException].
  Future<String> biometricSign(Uint8List data,
      {required String title, required String subtitle, required String cancel});
}

class NoDeviceKeys implements DeviceKeys {
  const NoDeviceKeys();
  @override
  Future<({String x, String y})?> dpopPublicKey() async => null;
  @override
  Future<Uint8List> dpopSign(Uint8List data) => throw UnsupportedError('no device keys');
  @override
  Future<void> dpopReset() async {}
  @override
  Future<bool> biometricAvailable() async => false;
  @override
  Future<bool> biometricEnrolled() async => false;
  @override
  Future<String> biometricCreate() => throw UnsupportedError('no device keys');
  @override
  Future<void> biometricDelete() async {}
  @override
  Future<String> biometricSign(Uint8List data,
          {required String title, required String subtitle, required String cancel}) =>
      throw BiometricException(BiometricOutcome.unavailable);
}

/// The Android implementation, over the "gebeya/device_keys" channel.
class ChannelDeviceKeys implements DeviceKeys {
  static const _ch = MethodChannel('gebeya/device_keys');
  bool _missing = false;
  ({String x, String y})? _jwk; // asked for on every request, so kept after the first call

  Future<T?> _call<T>(String method, [Map<String, Object?>? args]) async {
    if (_missing) return null;
    try {
      return await _ch.invokeMethod<T>(method, args);
    } on MissingPluginException {
      _missing = true; // this platform has no native side yet
      return null;
    }
  }

  @override
  Future<({String x, String y})?> dpopPublicKey() async {
    if (_jwk != null) return _jwk;
    final m = await _call<Map<Object?, Object?>>('dpopPublicKey');
    if (m == null) return null;
    return _jwk = (x: m['x'] as String, y: m['y'] as String);
  }

  @override
  Future<Uint8List> dpopSign(Uint8List data) async => (await _call<Uint8List>('dpopSign', {'data': data}))!;

  @override
  Future<void> dpopReset() {
    _jwk = null;
    return _call<void>('dpopReset');
  }

  @override
  Future<bool> biometricAvailable() async => (await _call<bool>('biometricAvailable')) ?? false;

  @override
  Future<bool> biometricEnrolled() async => (await _call<bool>('biometricEnrolled')) ?? false;

  @override
  Future<String> biometricCreate() async => (await _call<String>('biometricCreate'))!;

  @override
  Future<void> biometricDelete() => _call<void>('biometricDelete');

  @override
  Future<String> biometricSign(Uint8List data,
      {required String title, required String subtitle, required String cancel}) async {
    try {
      final sig =
          await _call<String>('biometricSign', {'data': data, 'title': title, 'subtitle': subtitle, 'cancel': cancel});
      if (sig == null) throw BiometricException(BiometricOutcome.unavailable);
      return sig;
    } on PlatformException catch (e) {
      throw BiometricException(switch (e.code) {
        'cancelled' => BiometricOutcome.cancelled,
        'locked_out' => BiometricOutcome.lockedOut,
        'not_enrolled' => BiometricOutcome.notEnrolled,
        _ => BiometricOutcome.unavailable,
      });
    }
  }
}

String _enc(List<int> b) => base64Url.encode(b).replaceAll('=', '');

/// Builds a DPoP proof (RFC 9449) for one request, or returns null when the device has no key.
/// [url] must be scheme://host[:port]/path, without query, as the server sees it.
Future<String?> dpopProof(DeviceKeys keys, String method, String url, {String? accessToken}) async {
  final jwk = await keys.dpopPublicKey();
  if (jwk == null) return null;
  final header = {
    'typ': 'dpop+jwt',
    'alg': 'ES256',
    'jwk': {'kty': 'EC', 'crv': 'P-256', 'x': jwk.x, 'y': jwk.y},
  };
  final claims = {
    'jti': newIdempotencyKey(),
    'htm': method,
    'htu': url,
    'iat': DateTime.now().millisecondsSinceEpoch ~/ 1000,
    if (accessToken != null) 'ath': _enc(sha256.convert(ascii.encode(accessToken)).bytes),
  };
  final signing = '${_enc(utf8.encode(jsonEncode(header)))}.${_enc(utf8.encode(jsonEncode(claims)))}';
  final der = await keys.dpopSign(Uint8List.fromList(ascii.encode(signing)));
  return '$signing.${_enc(derToRaw(der))}';
}

/// JWS ES256 wants r||s (64 bytes); the keystore returns ASN.1 DER SEQUENCE { INTEGER r, INTEGER s }.
Uint8List derToRaw(Uint8List der) {
  var i = 0;
  int len() {
    var l = der[i++];
    if (l & 0x80 != 0) {
      final n = l & 0x7f;
      l = 0;
      for (var k = 0; k < n; k++) {
        l = (l << 8) | der[i++];
      }
    }
    return l;
  }

  if (der[i++] != 0x30) throw const FormatException('not a DER sequence');
  len();
  final out = Uint8List(64);
  for (var part = 0; part < 2; part++) {
    if (der[i++] != 0x02) throw const FormatException('not a DER integer');
    final l = len();
    var v = der.sublist(i, i + l);
    i += l;
    while (v.length > 32 && v.first == 0) {
      v = v.sublist(1);
    }
    if (v.length > 32) throw const FormatException('integer too long');
    out.setRange(part * 32 + 32 - v.length, part * 32 + 32, v);
  }
  return out;
}
