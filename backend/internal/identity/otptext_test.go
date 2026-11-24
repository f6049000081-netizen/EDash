package identity

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The sign-in code fits in one SMS in every language: 70 characters in Ge'ez script (sent as UCS-2),
// 160 in Latin script.
func TestSignInSMSFitsOneMessage(t *testing.T) {
	for lang, max := range map[string]int{"en": 160, "om": 160, "am": 70, "ti": 70} {
		s := otpText(lang, "123456")
		if !strings.Contains(s, "123456") || utf8.RuneCountInString(s) > max {
			t.Errorf("%s (%d characters): %s", lang, utf8.RuneCountInString(s), s)
		}
	}
	if otpText("om", "1") == otpText("en", "1") || otpText("ti", "1") == otpText("en", "1") {
		t.Error("Afaan Oromo or Tigrinya falls back to English")
	}
}
