import 'dart:convert';
import 'dart:typed_data';

import 'package:crypto/crypto.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'package:gebeya_one/core/api.dart';
import 'package:gebeya_one/core/device_keys.dart';

/// A stand-in for the phone's keystore: a fixed public key and a fixed DER signature. The real
/// signatures are checked by the server's tests; here we check what the app puts around them.
class FakeKeys implements DeviceKeys {
  final signed = <String>[];
  int resets = 0;

  // r has a leading 0x00 (high bit set), s is short (31 bytes): both cases DER produces.
  static final der = Uint8List.fromList([
    0x30, 0x44, //
    0x02, 0x21, 0x00, ...List.filled(32, 0xAA),
    0x02, 0x1F, ...List.filled(31, 0x11),
  ]);

  @override
  Future<({String x, String y})?> dpopPublicKey() async => (x: 'X' * 43, y: 'Y' * 43);
  @override
  Future<Uint8List> dpopSign(Uint8List data) async {
    signed.add(ascii.decode(data));
    return der;
  }

  @override
  Future<void> dpopReset() async => resets++;
  @override
  Future<bool> biometricAvailable() async => true;
  @override
  Future<bool> biometricEnrolled() async => true;
  @override
  Future<String> biometricCreate() async => 'pub';
  @override
  Future<void> biometricDelete() async {}
  @override
  Future<String> biometricSign(Uint8List data,
          {required String title, required String subtitle, required String cancel}) async =>
      'sig';
}

Map<String, dynamic> part(String jwt, int i) =>
    jsonDecode(utf8.decode(base64Url.decode(base64Url.normalize(jwt.split('.')[i])))) as Map<String, dynamic>;

void main() {
  test('DER signatures become 64-byte r||s for JWS', () {
    final raw = derToRaw(FakeKeys.der);
    expect(raw.length, 64);
    expect(raw.sublist(0, 32), List.filled(32, 0xAA));
    expect(raw[32], 0); // s padded on the left
    expect(raw.sublist(33), List.filled(31, 0x11));
    expect(() => derToRaw(Uint8List.fromList([0x31, 0x00])), throwsFormatException);
  });

  test('a DPoP proof names the request, the key and the token', () async {
    final keys = FakeKeys();
    final proof = (await dpopProof(keys, 'GET', 'http://api:8080/v1/me', accessToken: 'tok'))!;
    final h = part(proof, 0), c = part(proof, 1);
    expect(h['typ'], 'dpop+jwt');
    expect(h['alg'], 'ES256');
    expect((h['jwk'] as Map)['crv'], 'P-256');
    expect(c['htm'], 'GET');
    expect(c['htu'], 'http://api:8080/v1/me');
    expect(c['ath'], base64Url.encode(sha256.convert(ascii.encode('tok')).bytes).replaceAll('=', ''));
    expect((c['jti'] as String).length, greaterThan(20));
    expect(base64Url.decode(base64Url.normalize(proof.split('.')[2])).length, 64);
    expect(keys.signed.single, proof.substring(0, proof.lastIndexOf('.')));
    expect(await dpopProof(const NoDeviceKeys(), 'GET', 'http://api/v1/me'), isNull);
  });

  test('sign-in binds the session; a DPoP session signs every request', () async {
    final keys = FakeKeys();
    final seen = <http.BaseRequest>[];
    final api = GebeyaApi(Api(
      baseUrl: 'http://api:8080',
      store: MemoryTokenStore(),
      keys: keys,
      client: MockClient((r) async {
        seen.add(r);
        return switch (r.url.path) {
          '/v1/auth/otp/verify' => http.Response(
              jsonEncode({
                'access_token': 'at1',
                'refresh_token': 'rt1',
                'token_type': 'DPoP',
                'user_id': 'u1',
                'expires_in': 900,
                'needs_pin': false,
              }),
              200),
          _ => http.Response(jsonEncode({'balances': []}), 200),
        };
      }),
    ));
    await api.verifyOtp('c1', '123456', platform: 'android', model: 'x');
    final verify = part(seen[0].headers['DPoP']!, 1);
    expect(verify['htu'], 'http://api:8080/v1/auth/otp/verify');
    expect(verify.containsKey('ath'), isFalse);

    await api.api.get('/v1/wallet/balances', query: {'x': '1'});
    final req = seen[1];
    expect(req.headers['Authorization'], 'DPoP at1');
    final c = part(req.headers['DPoP']!, 1);
    expect(c['htu'], 'http://api:8080/v1/wallet/balances'); // no query string
    expect(c['ath'], isNotNull);

    await api.api.setTokens(null);
    expect(keys.resets, 1, reason: 'signing out drops the key so the next sign-in binds a new one');
  });

  test('without device keys the app keeps using bearer tokens', () async {
    final seen = <http.BaseRequest>[];
    final api = Api(
      baseUrl: 'http://api',
      store: MemoryTokenStore(),
      client: MockClient((r) async {
        seen.add(r);
        return http.Response('{}', 200);
      }),
    );
    await api.setTokens(Tokens('at', 'rt', 'u', DateTime.now().add(const Duration(minutes: 10))));
    await api.get('/v1/me');
    expect(seen.single.headers['Authorization'], 'Bearer at');
    expect(seen.single.headers.containsKey('DPoP'), isFalse);
  });
}
