package notify

import "strings"

// Notification copy, written to the brand book: plain, specific, no exclamation marks, and it says what
// to do next. English and Amharic are written; Afaan Oromo and Tigrinya fall back to English until
// translators supply them (the same gap as the SMS text, listed in backend/README.md).
var texts = map[string]map[string]string{
	"en": {
		"money_received.title":    "Money received",
		"money_received.body":     "{name} sent you {amount}.",
		"payment_succeeded.title": "Payment complete",
		"payment_succeeded.body":  "Your payment of {amount} to {name} went through.",
		"payment_failed.title":    "Payment didn't go through",
		"payment_failed.body":     "Your payment of {amount} to {name} didn't go through. Your money wasn't taken.",
		"topup_succeeded.title":   "Money added",
		"topup_succeeded.body":    "{amount} from {rail} was added to your wallet.",
		"topup_failed.title":      "Top-up didn't go through",
		"topup_failed.body":       "Your top-up of {amount} from {rail} didn't complete, and nothing was added to your wallet.",
		"topup_held.title":        "We're checking your top-up",
		"topup_held.body":         "Your top-up of {amount} from {rail} is being reviewed. We'll tell you when it's done.",
		"new_device.title":        "New sign-in",
		"new_device.body":         "Your account was signed in on a new {platform} device. If this wasn't you, reset your PIN now.",
		"pin_reset.title":         "Your PIN was reset",
		"pin_reset.body":          "Your PIN was reset and your other devices were signed out. If you didn't do this, contact support.",
		"pin_changed.title":       "Your PIN was changed",
		"pin_changed.body":        "Your PIN was changed. If you didn't do this, reset your PIN and contact support.",
		"session_revoked.title":   "We signed out a device",
		"session_revoked.body":    "A sign-in on one of your devices looked unsafe, so we ended it. Sign in again on that device.",
	},
	"am": {
		"money_received.title":    "ገንዘብ ደርሶዎታል",
		"money_received.body":     "{name} {amount} ልከውልዎታል።",
		"payment_succeeded.title": "ክፍያው ተጠናቋል",
		"payment_succeeded.body":  "ለ{name} የከፈሉት {amount} ተፈጽሟል።",
		"payment_failed.title":    "ክፍያው አልተሳካም",
		"payment_failed.body":     "ለ{name} የከፈሉት {amount} አልተፈጸመም። ገንዘብዎ አልተወሰደም።",
		"topup_succeeded.title":   "ገንዘብ ተጨምሯል",
		"topup_succeeded.body":    "ከ{rail} {amount} ወደ ዋሌትዎ ተጨምሯል።",
		"topup_failed.title":      "ገንዘብ መሙላቱ አልተሳካም",
		"topup_failed.body":       "ከ{rail} የሞሉት {amount} አልተጠናቀቀም፤ ወደ ዋሌትዎ ምንም አልተጨመረም።",
		"topup_held.title":        "የሞሉትን ገንዘብ እያረጋገጥን ነው",
		"topup_held.body":         "ከ{rail} የሞሉት {amount} እየተገመገመ ነው። ሲጠናቀቅ እናሳውቅዎታለን።",
		"new_device.title":        "አዲስ መግቢያ",
		"new_device.body":         "ሂሳብዎ በአዲስ {platform} መሣሪያ ላይ ገብቷል። ይህ እርስዎ ካልሆኑ አሁኑኑ ፒንዎን ይቀይሩ።",
		"pin_reset.title":         "ፒንዎ ተቀይሯል",
		"pin_reset.body":          "ፒንዎ በአዲስ ተተክቷል፤ ሌሎች መሣሪያዎችዎም ወጥተዋል። ይህን ያደረጉት እርስዎ ካልሆኑ ድጋፍ ሰጪዎችን ያነጋግሩ።",
		"pin_changed.title":       "ፒንዎ ተቀይሯል",
		"pin_changed.body":        "ፒንዎ ተቀይሯል። ይህን ያደረጉት እርስዎ ካልሆኑ ፒንዎን ይቀይሩና ድጋፍ ሰጪዎችን ያነጋግሩ።",
		"session_revoked.title":   "አንድ መሣሪያ እንዲወጣ አድርገናል",
		"session_revoked.body":    "በአንዱ መሣሪያዎ ላይ ያለው መግቢያ አስተማማኝ ስላልመሰለን አቋርጠነዋል። በዚያ መሣሪያ ላይ እንደገና ይግቡ።",
	},
}

func text(locale, key string, args map[string]string) string {
	s, ok := texts[locale][key]
	if !ok {
		s = texts["en"][key]
	}
	for k, v := range args {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}
