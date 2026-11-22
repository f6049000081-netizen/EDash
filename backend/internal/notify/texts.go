package notify

import "strings"

// Notification copy, written to the brand book: plain, specific, no exclamation marks, and it says what
// to do next. English and Amharic are written; Afaan Oromo and Tigrinya fall back to English until
// translators supply them (the same gap as the SMS text, listed in backend/README.md).
var texts = map[string]map[string]string{
	"en": {
		"money_received.title":                     "Money received",
		"money_received.body":                      "{name} sent you {amount}.",
		"payment_succeeded.title":                  "Payment complete",
		"payment_succeeded.body":                   "Your payment of {amount} to {name} went through.",
		"payment_failed.title":                     "Payment didn't go through",
		"payment_failed.body":                      "Your payment of {amount} to {name} didn't go through. Your money wasn't taken.",
		"topup_succeeded.title":                    "Money added",
		"topup_succeeded.body":                     "{amount} from {rail} was added to your wallet.",
		"topup_failed.title":                       "Top-up didn't go through",
		"topup_failed.body":                        "Your top-up of {amount} from {rail} didn't complete, and nothing was added to your wallet.",
		"topup_held.title":                         "We're checking your top-up",
		"topup_held.body":                          "Your top-up of {amount} from {rail} is being reviewed. We'll tell you when it's done.",
		"new_device.title":                         "New sign-in",
		"new_device.body":                          "Your account was signed in on a new {platform} device. If this wasn't you, reset your PIN now.",
		"pin_reset.title":                          "Your PIN was reset",
		"pin_reset.body":                           "Your PIN was reset and your other devices were signed out. If you didn't do this, contact support.",
		"pin_changed.title":                        "Your PIN was changed",
		"pin_changed.body":                         "Your PIN was changed. If you didn't do this, reset your PIN and contact support.",
		"session_revoked.title":                    "We signed out a device",
		"session_revoked.body":                     "A sign-in on one of your devices looked unsafe, so we ended it. Sign in again on that device.",
		"biometric_enrolled.title":                 "Fingerprint or face confirmation is on",
		"biometric_enrolled.body":                  "Payments can now be confirmed with a fingerprint or face check on one of your devices. If this wasn't you, reset your PIN now.",
		"payout_settled.title":                     "Payout sent",
		"payout_settled.body":                      "{amount} from {shop}'s sales was paid to {destination}.",
		"payout_on_hold.title":                     "Payout on hold",
		"payout_on_hold.body":                      "{shop}'s payout of {amount} is on hold. Our team is checking it.",
		"payout_on_hold_no_destination.title":      "Add a payout account",
		"payout_on_hold_no_destination.body":       "{shop} has {amount} waiting. Add a payout account to receive it.",
		"payout_on_hold_merchant_suspended.title":  "Payout on hold",
		"payout_on_hold_merchant_suspended.body":   "{shop}'s payout of {amount} is on hold while the shop's account is reviewed. The money is kept for you.",
		"payout_on_hold_rejected_by_partner.title": "Payout couldn't be delivered",
		"payout_on_hold_rejected_by_partner.body":  "{destination} didn't accept {shop}'s payout of {amount}. Check the payout account; the money is kept for you.",
		"payout_on_hold_too_many_failures.title":   "Payout delayed",
		"payout_on_hold_too_many_failures.body":    "{shop}'s payout of {amount} kept failing, so our team is checking it. The money is kept for you.",
		"payout_destination_changed.title":         "Payout account changed",
		"payout_destination_changed.body":          "Payouts for {shop} will go to {destination} from {when}. If you didn't make this change, contact support now.",
	},
	"am": {
		"money_received.title":                     "ገንዘብ ደርሶዎታል",
		"money_received.body":                      "{name} {amount} ልከውልዎታል።",
		"payment_succeeded.title":                  "ክፍያው ተጠናቋል",
		"payment_succeeded.body":                   "ለ{name} የከፈሉት {amount} ተፈጽሟል።",
		"payment_failed.title":                     "ክፍያው አልተሳካም",
		"payment_failed.body":                      "ለ{name} የከፈሉት {amount} አልተፈጸመም። ገንዘብዎ አልተወሰደም።",
		"topup_succeeded.title":                    "ገንዘብ ተጨምሯል",
		"topup_succeeded.body":                     "ከ{rail} {amount} ወደ ዋሌትዎ ተጨምሯል።",
		"topup_failed.title":                       "ገንዘብ መሙላቱ አልተሳካም",
		"topup_failed.body":                        "ከ{rail} የሞሉት {amount} አልተጠናቀቀም፤ ወደ ዋሌትዎ ምንም አልተጨመረም።",
		"topup_held.title":                         "የሞሉትን ገንዘብ እያረጋገጥን ነው",
		"topup_held.body":                          "ከ{rail} የሞሉት {amount} እየተገመገመ ነው። ሲጠናቀቅ እናሳውቅዎታለን።",
		"new_device.title":                         "አዲስ መግቢያ",
		"new_device.body":                          "ሂሳብዎ በአዲስ {platform} መሣሪያ ላይ ገብቷል። ይህ እርስዎ ካልሆኑ አሁኑኑ ፒንዎን ይቀይሩ።",
		"pin_reset.title":                          "ፒንዎ ተቀይሯል",
		"pin_reset.body":                           "ፒንዎ በአዲስ ተተክቷል፤ ሌሎች መሣሪያዎችዎም ወጥተዋል። ይህን ያደረጉት እርስዎ ካልሆኑ ድጋፍ ሰጪዎችን ያነጋግሩ።",
		"pin_changed.title":                        "ፒንዎ ተቀይሯል",
		"pin_changed.body":                         "ፒንዎ ተቀይሯል። ይህን ያደረጉት እርስዎ ካልሆኑ ፒንዎን ይቀይሩና ድጋፍ ሰጪዎችን ያነጋግሩ።",
		"session_revoked.title":                    "አንድ መሣሪያ እንዲወጣ አድርገናል",
		"session_revoked.body":                     "በአንዱ መሣሪያዎ ላይ ያለው መግቢያ አስተማማኝ ስላልመሰለን አቋርጠነዋል። በዚያ መሣሪያ ላይ እንደገና ይግቡ።",
		"biometric_enrolled.title":                 "በጣት አሻራ ወይም በፊት ማረጋገጥ ተከፍቷል",
		"biometric_enrolled.body":                  "ከመሣሪያዎችዎ በአንዱ ላይ ክፍያዎችን በጣት አሻራ ወይም በፊት ማረጋገጥ ይቻላል። ይህን ያደረጉት እርስዎ ካልሆኑ አሁኑኑ ፒንዎን ይቀይሩ።",
		"payout_settled.title":                     "ክፍያ ተልኳል",
		"payout_settled.body":                      "ከ{shop} ሽያጭ {amount} ወደ {destination} ተከፍሏል።",
		"payout_on_hold.title":                     "ክፍያው ቆሟል",
		"payout_on_hold.body":                      "የ{shop} {amount} ክፍያ ቆሟል። ቡድናችን እያጣራው ነው።",
		"payout_on_hold_no_destination.title":      "የክፍያ መቀበያ ሂሳብ ያክሉ",
		"payout_on_hold_no_destination.body":       "{shop} {amount} እየጠበቀ ነው። ለመቀበል የክፍያ መቀበያ ሂሳብ ያክሉ።",
		"payout_on_hold_merchant_suspended.title":  "ክፍያው ቆሟል",
		"payout_on_hold_merchant_suspended.body":   "የሱቁ ሂሳብ እየተገመገመ ስለሆነ የ{shop} {amount} ክፍያ ቆሟል። ገንዘቡ ለእርስዎ ተጠብቋል።",
		"payout_on_hold_rejected_by_partner.title": "ክፍያው አልደረሰም",
		"payout_on_hold_rejected_by_partner.body":  "{destination} የ{shop} {amount} ክፍያን አልተቀበለም። የክፍያ መቀበያ ሂሳቡን ያረጋግጡ፤ ገንዘቡ ለእርስዎ ተጠብቋል።",
		"payout_on_hold_too_many_failures.title":   "ክፍያው ዘግይቷል",
		"payout_on_hold_too_many_failures.body":    "የ{shop} {amount} ክፍያ በተደጋጋሚ ስላልተሳካ ቡድናችን እያጣራው ነው። ገንዘቡ ለእርስዎ ተጠብቋል።",
		"payout_destination_changed.title":         "የክፍያ መቀበያ ሂሳብ ተቀይሯል",
		"payout_destination_changed.body":          "ከ{when} ጀምሮ የ{shop} ክፍያዎች ወደ {destination} ይሄዳሉ። ይህን ለውጥ ያደረጉት እርስዎ ካልሆኑ አሁኑኑ ድጋፍ ሰጪዎችን ያነጋግሩ።",
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
