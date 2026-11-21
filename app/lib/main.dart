import 'package:flutter/cupertino.dart' show CupertinoLocalizations;
import 'package:flutter/material.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';

import 'core/api.dart';
import 'core/app_state.dart';
import 'l10n/strings.dart';
import 'screens/home.dart';
import 'screens/signin.dart';
import 'ui/theme.dart';

/// The API address is set at build time:
///   flutter run --dart-define=API_BASE_URL=http://10.0.2.2:8080
/// (10.0.2.2 is the host machine as seen from the Android emulator.)
const apiBaseUrl = String.fromEnvironment('API_BASE_URL', defaultValue: 'http://10.0.2.2:8080');

/// The refresh token lives in the Android Keystore / iOS Keychain, never in plain preferences.
class SecureTokenStore implements TokenStore {
  static const _key = 'g1.refresh_token';
  final _storage = const FlutterSecureStorage(aOptions: AndroidOptions(encryptedSharedPreferences: true));

  @override
  Future<String?> readRefresh() => _storage.read(key: _key);

  @override
  Future<void> writeRefresh(String? token) =>
      token == null ? _storage.delete(key: _key) : _storage.write(key: _key, value: token);

  @override
  Future<String> installId() async {
    const key = 'g1.install_id';
    final existing = await _storage.read(key: key);
    if (existing != null) return existing;
    final id = newIdempotencyKey();
    await _storage.write(key: key, value: id);
    return id;
  }
}

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  final state = AppState(GebeyaApi(Api(baseUrl: apiBaseUrl, store: SecureTokenStore())));
  state.start();
  runApp(GebeyaOneApp(state: state));
}

class GebeyaOneApp extends StatelessWidget {
  const GebeyaOneApp({super.key, required this.state});
  final AppState state;

  @override
  Widget build(BuildContext context) {
    return AppScope(
      state: state,
      child: ListenableBuilder(
        listenable: state,
        builder: (context, _) {
          final ethiopic = const {'am', 'ti'}.contains(state.locale.languageCode);
          return MaterialApp(
            title: 'Gebeya One',
            debugShowCheckedModeBanner: false,
            theme: g1Theme(Brightness.light, ethiopic: ethiopic),
            darkTheme: g1Theme(Brightness.dark, ethiopic: ethiopic),
            locale: state.locale,
            supportedLocales: S.supported,
            localizationsDelegates: const [
              SDelegate(),
              FallbackMaterialLocalizations(),
              FallbackCupertinoLocalizations(),
              GlobalMaterialLocalizations.delegate,
              GlobalCupertinoLocalizations.delegate,
              GlobalWidgetsLocalizations.delegate,
            ],
            home: switch (state.stage) {
              Stage.starting => const _Splash(),
              Stage.signedOut => const PhoneScreen(),
              Stage.needsPin => const PinSetupScreen(),
              Stage.ready => const Shell(),
            },
          );
        },
      ),
    );
  }
}

class _Splash extends StatelessWidget {
  const _Splash();

  @override
  Widget build(BuildContext context) => Scaffold(
        body: Center(
          child: Text('Gebeya One',
              style: Theme.of(context).textTheme.displayMedium?.copyWith(color: G1.c(context).brand)),
        ),
      );
}

/// Flutter ships Material and Cupertino translations for Amharic and English but not for
/// Afaan Oromo or Tigrinya. For those two, the framework's own labels (back, paste, and so on)
/// use English; all Gebeya One copy still comes from [S].
class FallbackMaterialLocalizations extends LocalizationsDelegate<MaterialLocalizations> {
  const FallbackMaterialLocalizations();

  @override
  bool isSupported(Locale locale) => !GlobalMaterialLocalizations.delegate.isSupported(locale);

  @override
  Future<MaterialLocalizations> load(Locale locale) => GlobalMaterialLocalizations.delegate.load(const Locale('en'));

  @override
  bool shouldReload(FallbackMaterialLocalizations old) => false;
}

class FallbackCupertinoLocalizations extends LocalizationsDelegate<CupertinoLocalizations> {
  const FallbackCupertinoLocalizations();

  @override
  bool isSupported(Locale locale) => !GlobalCupertinoLocalizations.delegate.isSupported(locale);

  @override
  Future<CupertinoLocalizations> load(Locale locale) => GlobalCupertinoLocalizations.delegate.load(const Locale('en'));

  @override
  bool shouldReload(FallbackCupertinoLocalizations old) => false;
}
