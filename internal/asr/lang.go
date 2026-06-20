package asr

import "strings"

// promptDictionary maps a language tag to the encoder's int64 prompt_index
// (config.json prompt_dictionary). "auto" (101) lets the model detect the
// language itself and is the default. Embedded here — like the reference — so the
// engine needs no sidecar config.json at run time. Several tags alias one index
// (en == en-US, etc.).
var promptDictionary = map[string]int64{
	"auto": 101,
	"en":   0, "en-US": 0, "en-GB": 1,
	"es": 3, "es-ES": 2, "es-US": 3,
	"zh-CN": 4, "zh-TW": 5, "zh": 4,
	"hi": 6, "hi-IN": 6,
	"ar": 7, "ar-AR": 7,
	"fr": 8, "fr-FR": 8, "fr-CA": 100,
	"de": 9, "de-DE": 9,
	"ja": 10, "ja-JP": 10,
	"ru": 11, "ru-RU": 11,
	"pt": 13, "pt-BR": 12, "pt-PT": 13,
	"ko": 14, "ko-KR": 14,
	"it": 15, "it-IT": 15,
	"nl": 16, "nl-NL": 16,
	"pl": 17, "pl-PL": 17,
	"tr": 18, "tr-TR": 18,
	"uk": 19, "uk-UA": 19,
	"ro": 20, "ro-RO": 20,
	"el": 21, "el-GR": 21,
	"cs": 22, "cs-CZ": 22,
	"hu": 23, "hu-HU": 23,
	"sv": 24, "sv-SE": 24,
	"da": 25, "da-DK": 25,
	"fi": 26, "fi-FI": 26,
	"no": 27, "nb": 103, "nn": 104,
	"sk": 28, "hr": 29, "bg": 30, "lt": 31,
	"th": 32, "vi": 33, "id": 34, "ms": 35,
	"et": 60, "lv": 61, "sl": 62, "he": 64,
}

// resolveLang maps a language tag to its prompt index, falling back to auto
// (101) for an empty or unknown tag. The tag is canonicalized to the dictionary's
// BCP-47 form first (lowercase language, UPPERCASE region, "_"->"-"), so case- and
// separator-variant inputs like "en-gb", "pt_br", or "ZH-cn" resolve to the
// region-specific prompt instead of silently degrading to the generic language.
// An unknown locale still falls back to the bare language (e.g. "en-AU" -> "en").
func resolveLang(tag string) int64 {
	if tag == "" {
		return promptAuto
	}
	canon := canonicalizeLang(tag)
	if idx, ok := promptDictionary[canon]; ok {
		return idx
	}
	if i := strings.IndexByte(canon, '-'); i > 0 {
		if idx, ok := promptDictionary[canon[:i]]; ok {
			return idx
		}
	}
	return promptAuto
}

// canonicalizeLang normalizes a language tag to the dictionary's form: "_"->"-",
// the language subtag lower-cased, the region subtag UPPER-cased. "en_us" ->
// "en-US", "ZH-cn" -> "zh-CN", "AUTO" -> "auto".
func canonicalizeLang(tag string) string {
	tag = strings.ReplaceAll(tag, "_", "-")
	lang, region, ok := strings.Cut(tag, "-")
	lang = strings.ToLower(lang)
	if !ok {
		return lang
	}
	return lang + "-" + strings.ToUpper(region)
}
