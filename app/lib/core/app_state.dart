import 'package:flutter/widgets.dart';

import 'api.dart';
import 'money.dart';

enum Stage { starting, signedOut, needsPin, ready }

/// Holds the session, the profile and the balance, and tells the UI when they change.
class AppState extends ChangeNotifier {
  AppState(this.api, {Locale? initialLocale}) : locale = initialLocale ?? const Locale('am') {
    api.api.onSignedOut = _signedOut;
  }

  final GebeyaApi api;

  Stage stage = Stage.starting;
  Locale locale;
  Profile? profile;
  Money? balance;
  bool balanceHidden = false;

  /// Activity changes after every payment or top-up; screens listening to this reload.
  int activityVersion = 0;

  Future<void> start() async {
    bool restored;
    try {
      restored = await api.api.restore();
    } on ApiProblem {
      restored = false; // offline at launch: show sign-in; the stored token is kept for next time
    }
    if (!restored) {
      stage = Stage.signedOut;
      notifyListeners();
      return;
    }
    await _afterSignIn();
  }

  /// Called after the SMS code is accepted.
  Future<void> signedIn({required bool needsPin}) async {
    if (needsPin) {
      stage = Stage.needsPin;
      notifyListeners();
      return;
    }
    await _afterSignIn();
  }

  Future<void> pinSet() => _afterSignIn();

  Future<void> _afterSignIn() async {
    try {
      final p = await api.me();
      profile = p;
      locale = Locale(p.locale);
      stage = p.hasPin ? Stage.ready : Stage.needsPin;
    } on ApiProblem catch (e) {
      if (e.status == 401) {
        stage = Stage.signedOut;
      } else {
        stage = Stage.ready; // show the app; screens show their own retry
      }
    }
    notifyListeners();
    if (stage == Stage.ready) await refresh();
  }

  /// Reloads the balance and profile (limits change after every payment).
  Future<void> refresh() async {
    try {
      final results = await Future.wait([api.balance(), api.me()]);
      balance = results[0] as Money;
      profile = results[1] as Profile;
      activityVersion++;
      notifyListeners();
    } on ApiProblem {
      // keep what we had; the home screen offers a retry when balance is still null
      notifyListeners();
    }
  }

  Future<void> setLocale(Locale l) async {
    locale = l;
    notifyListeners();
    if (stage == Stage.ready) {
      try {
        profile = await api.updateMe(locale: l.languageCode);
        notifyListeners();
      } on ApiProblem {
        // the UI language already changed; the server copy (for SMS language) syncs next time
      }
    }
  }

  void toggleBalance() {
    balanceHidden = !balanceHidden;
    notifyListeners();
  }

  Future<void> signOut() async {
    try {
      await api.logout();
    } on ApiProblem {
      // the local session is cleared either way
    }
    _signedOut();
  }

  void _signedOut() {
    profile = null;
    balance = null;
    stage = Stage.signedOut;
    notifyListeners();
  }
}

/// Makes [AppState] available to the widget tree without a state-management package.
class AppScope extends InheritedNotifier<AppState> {
  const AppScope({super.key, required AppState state, required super.child}) : super(notifier: state);

  static AppState of(BuildContext context) => context.dependOnInheritedWidgetOfExactType<AppScope>()!.notifier!;

  /// For callbacks that must not rebuild when the state changes.
  static AppState read(BuildContext context) => context.getInheritedWidgetOfExactType<AppScope>()!.notifier!;
}
