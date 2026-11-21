import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../core/api.dart';
import '../core/money.dart';
import '../l10n/strings.dart';
import 'theme.dart';

/// Amount text: tabular figures, and a screen-reader label that says the currency in full.
class AmountText extends StatelessWidget {
  const AmountText(this.money, {super.key, this.style, this.prefix = ''});

  final Money money;
  final TextStyle? style;
  final String prefix;

  @override
  Widget build(BuildContext context) => Semantics(
        label: '$prefix${money.spoken()}',
        excludeSemantics: true,
        child: Text('$prefix${money.format()}',
            style: (style ?? Theme.of(context).textTheme.titleSmall)?.copyWith(fontFeatures: G1.tabular)),
      );
}

/// The wallet balance card: brand-strong ground, hide/show toggle.
class BalanceCard extends StatelessWidget {
  const BalanceCard({super.key, required this.balance, required this.hidden, required this.onToggle, this.onRetry});

  final Money? balance;
  final bool hidden;
  final VoidCallback onToggle;
  final VoidCallback? onRetry;

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    final fg = Theme.of(context).brightness == Brightness.light ? c.onBrand : c.ink;
    final bg = Theme.of(context).brightness == Brightness.light ? c.brandStrong : c.brandTint;
    return Container(
      padding: const EdgeInsets.all(G1.s5),
      decoration: BoxDecoration(color: bg, borderRadius: BorderRadius.circular(G1.rL), boxShadow: G1.shadow1(context)),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(children: [
            Expanded(child: Text(s.t('balance'), style: text.bodySmall?.copyWith(color: fg.withOpacity(0.85)))),
            IconButton(
              onPressed: onToggle,
              tooltip: hidden ? s.t('show_balance') : s.t('hide_balance'),
              icon: Icon(hidden ? Icons.visibility_off_outlined : Icons.visibility_outlined, color: fg, size: 22),
            ),
          ]),
          const SizedBox(height: G1.s1),
          if (balance == null)
            onRetry == null
                ? SizedBox(height: 40, child: Align(alignment: Alignment.centerLeft, child: _skeleton(fg)))
                : TextButton.icon(
                    onPressed: onRetry,
                    style: TextButton.styleFrom(foregroundColor: fg, padding: EdgeInsets.zero),
                    icon: const Icon(Icons.refresh),
                    label: Text(s.t('retry')))
          else if (hidden)
            Semantics(
              label: s.t('balance_hidden'),
              child: Text('ETB ••••••', style: text.displayLarge?.copyWith(color: fg)),
            )
          else
            AmountText(balance!, style: text.displayLarge?.copyWith(color: fg)),
        ],
      ),
    );
  }

  Widget _skeleton(Color fg) => Container(
      width: 180, height: 28, decoration: BoxDecoration(color: fg.withOpacity(0.15), borderRadius: BorderRadius.circular(G1.rS)));
}

/// A quick action: icon in a 48px brand-tint well, label underneath.
class QuickAction extends StatelessWidget {
  const QuickAction({super.key, required this.icon, required this.label, required this.onTap});

  final IconData icon;
  final String label;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    return Semantics(
      button: true,
      label: label,
      excludeSemantics: true,
      child: InkWell(
        onTap: onTap,
        borderRadius: BorderRadius.circular(G1.rM),
        child: Padding(
          padding: const EdgeInsets.symmetric(vertical: G1.s2),
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            Container(
              width: 48,
              height: 48,
              decoration: BoxDecoration(color: c.brandTint, borderRadius: BorderRadius.circular(G1.rM)),
              child: Icon(icon, color: c.brand),
            ),
            const SizedBox(height: G1.s2),
            Text(label,
                textAlign: TextAlign.center,
                maxLines: 2,
                style: Theme.of(context).textTheme.bodySmall?.copyWith(color: c.ink)),
          ]),
        ),
      ),
    );
  }
}

/// One activity line: 40px leading icon, two lines of text, trailing amount.
/// Money in is green with a plus; money out stays ink (spending is not an error).
class TransactionRow extends StatelessWidget {
  const TransactionRow({super.key, required this.item, this.onTap});

  final ActivityItem item;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    final icon = switch (item.type) {
      'topup' => Icons.add_card_outlined,
      'payment' => Icons.storefront_outlined,
      'transfer_in' => Icons.south_west,
      _ => Icons.north_east,
    };
    final incoming = item.direction == 'in';
    final amountColor = switch (item.status) {
      'failed' => c.inkMuted,
      _ => incoming ? c.success : c.ink,
    };
    final meta = [s.t('type_${item.type}'), formatWhen(context, item.occurredAt)];
    return InkWell(
      onTap: onTap,
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: G1.s4, vertical: G1.s3),
        child: Row(children: [
          Container(
            width: 40,
            height: 40,
            decoration: BoxDecoration(color: c.surface2, shape: BoxShape.circle),
            child: Icon(icon, size: 20, color: c.ink),
          ),
          const SizedBox(width: G1.s3),
          Expanded(
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              Text(item.title.isEmpty ? s.t('type_${item.type}') : item.title,
                  style: text.titleSmall, maxLines: 1, overflow: TextOverflow.ellipsis),
              const SizedBox(height: 2),
              Text(meta.join(' · '), style: text.bodySmall, maxLines: 1, overflow: TextOverflow.ellipsis),
            ]),
          ),
          const SizedBox(width: G1.s3),
          Column(crossAxisAlignment: CrossAxisAlignment.end, children: [
            AmountText(item.amount,
                prefix: incoming ? '+' : '',
                style: text.titleSmall?.copyWith(
                    color: amountColor, decoration: item.status == 'failed' ? TextDecoration.lineThrough : null)),
            if (item.status != 'completed') ...[
              const SizedBox(height: 2),
              StatusPill(item.status),
            ],
          ]),
        ]),
      ),
    );
  }
}

/// Status always has a word as well as a colour.
class StatusPill extends StatelessWidget {
  const StatusPill(this.status, {super.key});
  final String status;

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final (bg, fg) = switch (status) {
      'failed' => (c.dangerTint, c.danger),
      'pending' => (c.warningTint, c.warning),
      'refunded' => (c.surface2, c.inkMuted),
      _ => (c.successTint, c.success),
    };
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: G1.s2, vertical: 1),
      decoration: BoxDecoration(color: bg, borderRadius: BorderRadius.circular(G1.rPill)),
      child: Text(S.of(context).t('status_$status'),
          style: Theme.of(context).textTheme.labelSmall?.copyWith(color: fg, letterSpacing: 0)),
    );
  }
}

/// "Today, 08:42", "Yesterday, 19:10", "18/11/2026, 08:42".
/// The brand book asks for "Mon 18 Nov"; weekday and month names come with the intl date
/// formats once Afaan Oromo and Tigrinya data is reviewed, so dates are numeric for now.
String formatWhen(BuildContext context, DateTime t) {
  final s = S.of(context);
  final now = DateTime.now();
  final day = DateTime(t.year, t.month, t.day);
  final today = DateTime(now.year, now.month, now.day);
  String two(int n) => n.toString().padLeft(2, '0');
  final hm = '${two(t.hour)}:${two(t.minute)}';
  if (day == today) return '${s.t('today')}, $hm';
  if (day == today.subtract(const Duration(days: 1))) return '${s.t('yesterday')}, $hm';
  return '${two(t.day)}/${two(t.month)}/${t.year}, $hm';
}

/// A 6-digit PIN entry: dots plus a number pad. Never shows digits. Calls [onComplete] at 6 digits.
class PinPad extends StatefulWidget {
  const PinPad({super.key, required this.onComplete, this.error, this.busy = false});

  final Future<void> Function(String pin) onComplete;
  final String? error;
  final bool busy;

  @override
  State<PinPad> createState() => _PinPadState();
}

class _PinPadState extends State<PinPad> {
  String _pin = '';

  void _tap(String d) async {
    if (widget.busy || _pin.length >= 6) return;
    HapticFeedback.selectionClick();
    setState(() => _pin += d);
    if (_pin.length == 6) {
      final pin = _pin;
      await widget.onComplete(pin);
      if (mounted) setState(() => _pin = '');
    }
  }

  void _back() {
    if (_pin.isEmpty || widget.busy) return;
    setState(() => _pin = _pin.substring(0, _pin.length - 1));
  }

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final text = Theme.of(context).textTheme;
    Widget key(String label, {VoidCallback? onTap, Widget? child, String? semantic}) => Expanded(
          child: Semantics(
            button: true,
            label: semantic ?? label,
            excludeSemantics: true,
            child: InkWell(
              onTap: onTap,
              borderRadius: BorderRadius.circular(G1.rPill),
              child: SizedBox(
                height: 64,
                child: Center(child: child ?? Text(label, style: text.displayMedium?.copyWith(fontWeight: FontWeight.w600))),
              ),
            ),
          ),
        );
    return Column(mainAxisSize: MainAxisSize.min, children: [
      Semantics(
        label: '${_pin.length} of 6',
        liveRegion: true,
        child: Row(
          mainAxisAlignment: MainAxisAlignment.center,
          children: List.generate(
            6,
            (i) => AnimatedContainer(
              duration: const Duration(milliseconds: 180),
              curve: Curves.easeOut,
              margin: const EdgeInsets.symmetric(horizontal: G1.s2),
              width: 14,
              height: 14,
              decoration: BoxDecoration(
                shape: BoxShape.circle,
                color: i < _pin.length ? c.brand : Colors.transparent,
                border: Border.all(color: i < _pin.length ? c.brand : c.lineStrong, width: 1.5),
              ),
            ),
          ),
        ),
      ),
      SizedBox(
        height: 48,
        child: Center(
          child: widget.busy
              ? const SizedBox(width: 22, height: 22, child: CircularProgressIndicator(strokeWidth: 2))
              : widget.error == null
                  ? null
                  : Padding(
                      padding: const EdgeInsets.symmetric(horizontal: G1.s4),
                      child: Text(widget.error!,
                          textAlign: TextAlign.center, style: text.bodySmall?.copyWith(color: c.danger)),
                    ),
        ),
      ),
      for (final row in const [
        ['1', '2', '3'],
        ['4', '5', '6'],
        ['7', '8', '9'],
      ])
        Row(children: [for (final d in row) key(d, onTap: () => _tap(d))]),
      Row(children: [
        const Expanded(child: SizedBox(height: 64)),
        key('0', onTap: () => _tap('0')),
        key('', onTap: _back, semantic: 'Delete', child: Icon(Icons.backspace_outlined, color: c.ink)),
      ]),
    ]);
  }
}

/// A calm inline error: says what went wrong and, for payments, whether money was taken.
class ProblemBanner extends StatelessWidget {
  const ProblemBanner(this.problem, {super.key, this.onRetry});
  final ApiProblem problem;
  final VoidCallback? onRetry;

  @override
  Widget build(BuildContext context) {
    final c = G1.c(context);
    final s = S.of(context);
    final text = Theme.of(context).textTheme;
    final msg = problem.message.isNotEmpty ? problem.message : s.t('error_generic');
    return Container(
      width: double.infinity,
      padding: const EdgeInsets.all(G1.s4),
      decoration: BoxDecoration(color: c.dangerTint, borderRadius: BorderRadius.circular(G1.rM)),
      child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Icon(Icons.error_outline, color: c.danger, size: 22),
        const SizedBox(width: G1.s3),
        Expanded(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(msg, style: text.bodyMedium),
            if (problem.charged == false) Text(s.t('not_charged'), style: text.bodySmall?.copyWith(color: c.ink)),
            if (onRetry != null)
              TextButton(
                  onPressed: onRetry,
                  style: TextButton.styleFrom(padding: EdgeInsets.zero),
                  child: Text(s.t('retry'))),
          ]),
        ),
      ]),
    );
  }
}

/// Primary button with a busy state. One per screen.
class PrimaryButton extends StatelessWidget {
  const PrimaryButton({super.key, required this.label, required this.onPressed, this.busy = false});
  final String label;
  final VoidCallback? onPressed;
  final bool busy;

  @override
  Widget build(BuildContext context) => FilledButton(
        onPressed: busy ? null : onPressed,
        child: busy
            ? const SizedBox(width: 22, height: 22, child: CircularProgressIndicator(strokeWidth: 2))
            : Text(label),
      );
}
