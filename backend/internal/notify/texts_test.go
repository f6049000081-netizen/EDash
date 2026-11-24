package notify

import (
	"regexp"
	"slices"
	"testing"
)

var placeholder = regexp.MustCompile(`\{[a-z]+\}`)

// Every language has every text, with the same placeholders, so nobody is sent English or a raw {name}.
func TestEveryLanguageIsComplete(t *testing.T) {
	for _, lang := range []string{"am", "om", "ti"} {
		for key, en := range texts["en"] {
			v, ok := texts[lang][key]
			if !ok {
				t.Errorf("%s: %s missing", lang, key)
				continue
			}
			want, got := placeholder.FindAllString(en, -1), placeholder.FindAllString(v, -1)
			slices.Sort(want)
			slices.Sort(got)
			if !slices.Equal(want, got) {
				t.Errorf("%s: %s has %v, English has %v", lang, key, got, want)
			}
		}
		if len(texts[lang]) != len(texts["en"]) {
			t.Errorf("%s has %d texts, English %d", lang, len(texts[lang]), len(texts["en"]))
		}
	}
	if got := text("om", "team_invited.body", map[string]string{"name": "Hana", "role": "cashier"}); !regexp.MustCompile(`Hana.*kaashiyera`).MatchString(got) {
		t.Errorf("role in Afaan Oromo: %s", got)
	}
}
