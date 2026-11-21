import 'package:flutter/material.dart';

/// Gebeya One design tokens, copied value for value from the design system (tokens.json).
@immutable
class G1Colors extends ThemeExtension<G1Colors> {
  const G1Colors({
    required this.brand,
    required this.brandStrong,
    required this.brandTint,
    required this.onBrand,
    required this.accent,
    required this.accentTint,
    required this.onAccent,
    required this.ink,
    required this.inkMuted,
    required this.inkSubtle,
    required this.surface0,
    required this.surface1,
    required this.surface2,
    required this.line,
    required this.lineStrong,
    required this.success,
    required this.successTint,
    required this.warning,
    required this.warningTint,
    required this.danger,
    required this.dangerTint,
  });

  final Color brand, brandStrong, brandTint, onBrand;
  final Color accent, accentTint, onAccent;
  final Color ink, inkMuted, inkSubtle;
  final Color surface0, surface1, surface2, line, lineStrong;
  final Color success, successTint, warning, warningTint, danger, dangerTint;

  static const light = G1Colors(
    brand: Color(0xFF0E5A63),
    brandStrong: Color(0xFF0A434A),
    brandTint: Color(0xFFE3F1F1),
    onBrand: Color(0xFFFFFFFF),
    accent: Color(0xFFE8A317),
    accentTint: Color(0xFFFCF1D9),
    onAccent: Color(0xFF2A1D05),
    ink: Color(0xFF14201F),
    inkMuted: Color(0xFF566462),
    inkSubtle: Color(0xFF7D8A88),
    surface0: Color(0xFFF5F7F6),
    surface1: Color(0xFFFFFFFF),
    surface2: Color(0xFFEDF1F0),
    line: Color(0xFFDCE3E1),
    lineStrong: Color(0xFF8A9896),
    success: Color(0xFF17804F),
    successTint: Color(0xFFE3F4EB),
    warning: Color(0xFF9A5B05),
    warningTint: Color(0xFFFDF3DC),
    danger: Color(0xFFC2362B),
    dangerTint: Color(0xFFFBE6E4),
  );

  static const dark = G1Colors(
    brand: Color(0xFF4DB6BE),
    brandStrong: Color(0xFF7FD0D5),
    brandTint: Color(0xFF12343A),
    onBrand: Color(0xFF06282C),
    accent: Color(0xFFF2B93B),
    accentTint: Color(0xFF3A2E12),
    onAccent: Color(0xFF2A1D05),
    ink: Color(0xFFEEF3F2),
    inkMuted: Color(0xFF9CABAA),
    inkSubtle: Color(0xFF74827F),
    surface0: Color(0xFF0B1112),
    surface1: Color(0xFF141C1D),
    surface2: Color(0xFF1C2627),
    line: Color(0xFF263233),
    lineStrong: Color(0xFF5D6C6A),
    success: Color(0xFF4CC38A),
    successTint: Color(0xFF10301F),
    warning: Color(0xFFF0B44C),
    warningTint: Color(0xFF33270F),
    danger: Color(0xFFF2776B),
    dangerTint: Color(0xFF3A1715),
  );

  @override
  G1Colors copyWith() => this;

  @override
  G1Colors lerp(G1Colors? other, double t) => t < 0.5 ? this : (other ?? this);
}

/// Spacing (4px grid) and radii from the design system.
class G1 {
  static const s1 = 4.0, s2 = 8.0, s3 = 12.0, s4 = 16.0, s5 = 20.0, s6 = 24.0, s8 = 32.0, s10 = 40.0;
  static const rS = 8.0, rM = 14.0, rL = 20.0, rXl = 28.0, rPill = 999.0;

  static G1Colors c(BuildContext context) => Theme.of(context).extension<G1Colors>()!;

  static const tabular = [FontFeature.tabularFigures()];

  static List<BoxShadow> shadow1(BuildContext context) => Theme.of(context).brightness == Brightness.light
      ? const [
          BoxShadow(color: Color(0x0F0A282C), offset: Offset(0, 1), blurRadius: 2),
          BoxShadow(color: Color(0x0A0A282C), offset: Offset(0, 1), blurRadius: 1),
        ]
      : const [BoxShadow(color: Color(0x80000000), offset: Offset(0, 1), blurRadius: 2)];

  static List<BoxShadow> shadow2(BuildContext context) => Theme.of(context).brightness == Brightness.light
      ? const [BoxShadow(color: Color(0x1F0A282C), offset: Offset(0, 10), blurRadius: 30)]
      : const [BoxShadow(color: Color(0x8C000000), offset: Offset(0, 10), blurRadius: 30)];
}

const _display = 'Manrope';
const _sans = 'Inter';
const _ethiopic = 'NotoSansEthiopic';
const _fallback = [_ethiopic];

/// Text styles from the design system. In Amharic and Tigrinya, body and title text switch to the
/// Ethiopic styles: one step larger with taller lines.
TextTheme _textTheme(G1Colors c, bool ethiopic) {
  TextStyle st(String family, double size, double height, FontWeight w, {double ls = 0, Color? color}) => TextStyle(
        fontFamily: family,
        fontFamilyFallback: family == _ethiopic ? const [_sans] : _fallback,
        fontSize: size,
        height: height / size,
        fontWeight: w,
        letterSpacing: ls * size,
        color: color ?? c.ink,
      );
  return TextTheme(
    // amount-hero
    displayLarge: st(_display, 36, 40, FontWeight.w700, ls: -0.02).copyWith(fontFeatures: G1.tabular),
    // display
    displayMedium: st(_display, 28, 34, FontWeight.w800, ls: -0.02),
    // title / title-ethiopic
    titleLarge: ethiopic ? st(_ethiopic, 21, 30, FontWeight.w600) : st(_display, 20, 26, FontWeight.w700, ls: -0.01),
    // headline
    titleMedium: st(_display, 16, 22, FontWeight.w700),
    // body / body-ethiopic
    bodyLarge: ethiopic ? st(_ethiopic, 16, 26, FontWeight.w400) : st(_sans, 15, 22, FontWeight.w400),
    bodyMedium: ethiopic ? st(_ethiopic, 16, 26, FontWeight.w400) : st(_sans, 15, 22, FontWeight.w400),
    // body-strong
    titleSmall: ethiopic ? st(_ethiopic, 16, 26, FontWeight.w600) : st(_sans, 15, 22, FontWeight.w600),
    // caption
    bodySmall: st(ethiopic ? _ethiopic : _sans, 13, 18, FontWeight.w400, color: c.inkMuted),
    // label
    labelSmall: st(ethiopic ? _ethiopic : _sans, 12, 16, FontWeight.w600, ls: ethiopic ? 0 : 0.04, color: c.inkMuted),
    labelLarge: st(ethiopic ? _ethiopic : _sans, 15, 22, FontWeight.w600),
  );
}

ThemeData g1Theme(Brightness b, {required bool ethiopic}) {
  final c = b == Brightness.light ? G1Colors.light : G1Colors.dark;
  final text = _textTheme(c, ethiopic);
  final scheme = ColorScheme(
    brightness: b,
    primary: c.brand,
    onPrimary: c.onBrand,
    secondary: c.accent,
    onSecondary: c.onAccent,
    error: c.danger,
    onError: c.onBrand,
    surface: c.surface1,
    onSurface: c.ink,
    surfaceContainerHighest: c.surface2,
    outline: c.lineStrong,
    outlineVariant: c.line,
  );
  final shape = RoundedRectangleBorder(borderRadius: BorderRadius.circular(G1.rM));
  return ThemeData(
    useMaterial3: true,
    brightness: b,
    colorScheme: scheme,
    scaffoldBackgroundColor: c.surface0,
    textTheme: text,
    extensions: [c],
    splashFactory: InkRipple.splashFactory,
    appBarTheme: AppBarTheme(
      backgroundColor: c.surface0,
      foregroundColor: c.ink,
      elevation: 0,
      scrolledUnderElevation: 0,
      centerTitle: false,
      titleTextStyle: text.titleLarge,
    ),
    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(
        backgroundColor: c.brand,
        foregroundColor: c.onBrand,
        disabledBackgroundColor: c.surface2,
        disabledForegroundColor: c.inkSubtle,
        minimumSize: const Size.fromHeight(52),
        shape: shape,
        textStyle: text.labelLarge,
      ),
    ),
    outlinedButtonTheme: OutlinedButtonThemeData(
      style: OutlinedButton.styleFrom(
        foregroundColor: c.brand,
        side: BorderSide(color: c.line),
        minimumSize: const Size.fromHeight(52),
        shape: shape,
        textStyle: text.labelLarge,
      ),
    ),
    textButtonTheme: TextButtonThemeData(
      style:
          TextButton.styleFrom(foregroundColor: c.brand, minimumSize: const Size(44, 44), textStyle: text.labelLarge),
    ),
    inputDecorationTheme: InputDecorationTheme(
      filled: true,
      fillColor: c.surface2,
      contentPadding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s4),
      border:
          OutlineInputBorder(borderRadius: BorderRadius.circular(G1.rM), borderSide: BorderSide(color: c.lineStrong)),
      enabledBorder:
          OutlineInputBorder(borderRadius: BorderRadius.circular(G1.rM), borderSide: BorderSide(color: c.lineStrong)),
      focusedBorder: OutlineInputBorder(
          borderRadius: BorderRadius.circular(G1.rM), borderSide: BorderSide(color: c.brand, width: 2)),
      errorBorder:
          OutlineInputBorder(borderRadius: BorderRadius.circular(G1.rM), borderSide: BorderSide(color: c.danger)),
      labelStyle: text.bodyMedium?.copyWith(color: c.inkMuted),
      hintStyle: text.bodyMedium?.copyWith(color: c.inkSubtle),
    ),
    dividerTheme: DividerThemeData(color: c.line, thickness: 1, space: 1),
    snackBarTheme: SnackBarThemeData(
      behavior: SnackBarBehavior.floating,
      backgroundColor: c.ink,
      contentTextStyle: text.bodyMedium?.copyWith(color: c.surface1),
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(G1.rM)),
    ),
    pageTransitionsTheme: const PageTransitionsTheme(builders: {
      TargetPlatform.android: ZoomPageTransitionsBuilder(),
      TargetPlatform.iOS: CupertinoPageTransitionsBuilder(),
    }),
  );
}
