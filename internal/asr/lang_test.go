package asr

import "testing"

func TestResolveLang(t *testing.T) {
	cases := []struct {
		tag  string
		want int64
	}{
		{"", promptAuto},
		{"auto", 101},
		{"AUTO", 101},
		{"en", 0},
		{"en-US", 0},
		{"en-GB", 1},
		{"en-gb", 1},   // lowercase locale must still resolve to British English, not generic en
		{"EN-GB", 1},   // mixed case
		{"pt-BR", 12},  // Brazilian Portuguese
		{"pt-br", 12},  // lowercase locale
		{"pt_BR", 12},  // underscore separator
		{"pt", 13},     // generic -> European Portuguese
		{"zh-cn", 4},   // lowercase
		{"fr-ca", 100}, // Canadian French
		{"en-AU", 0},   // unknown locale -> bare language (en)
		{"xx", promptAuto},
		{"xx-YY", promptAuto},
	}
	for _, c := range cases {
		if got := resolveLang(c.tag); got != c.want {
			t.Errorf("resolveLang(%q) = %d, want %d", c.tag, got, c.want)
		}
	}
}

func TestCanonicalizeLang(t *testing.T) {
	cases := map[string]string{
		"en_us": "en-US",
		"ZH-cn": "zh-CN",
		"AUTO":  "auto",
		"en":    "en",
		"pt-BR": "pt-BR",
	}
	for in, want := range cases {
		if got := canonicalizeLang(in); got != want {
			t.Errorf("canonicalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}
