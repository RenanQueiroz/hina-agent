package automation

import (
	"encoding/json"
	"testing"
)

func mkScope(t *testing.T, js string) scope {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("bad scope json: %v", err)
	}
	return scope(m)
}

func TestResolveReference(t *testing.T) {
	sc := mkScope(t, `{
		"find": {"items": [{"pr": 12, "title": "a"}, {"pr": 34}]},
		"combine": {"markdown": "hello"},
		"item": {"pr": 7}
	}`)
	cases := []struct {
		ref  string
		want string // JSON of the resolved value
	}{
		{"find.items", `[{"pr":12,"title":"a"},{"pr":34}]`},
		{"find.items[0].pr", `12`},
		{"find.items[1].pr", `34`},
		{"combine.markdown", `"hello"`},
		{"item.pr", `7`},
	}
	for _, c := range cases {
		v, err := sc.resolve(c.ref)
		if err != nil {
			t.Fatalf("resolve(%q): %v", c.ref, err)
		}
		got, _ := json.Marshal(v)
		if string(got) != c.want {
			t.Errorf("resolve(%q) = %s, want %s", c.ref, got, c.want)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	sc := mkScope(t, `{"a": {"b": [1,2]}}`)
	for _, ref := range []string{"missing", "a.nope", "a.b[9]", "a.b.c", "a.b[0].x", "", "a..b", "a.", "a[0]"} {
		if _, err := sc.resolve(ref); err == nil {
			t.Errorf("resolve(%q) should have errored", ref)
		}
	}
}

func TestExpandTemplate(t *testing.T) {
	sc := mkScope(t, `{"item": {"pr": 7, "title": "x"}, "s": {"n": 3}}`)
	cases := []struct{ tmpl, want string }{
		{"PR ${item.pr}: ${item.title}", "PR 7: x"},
		{"n=${s.n}", "n=3"},
		{"whole=${item}", `whole={"pr":7,"title":"x"}`},
		{"no refs", "no refs"},
	}
	for _, c := range cases {
		got, err := sc.expand(c.tmpl)
		if err != nil {
			t.Fatalf("expand(%q): %v", c.tmpl, err)
		}
		if got != c.want {
			t.Errorf("expand(%q) = %q, want %q", c.tmpl, got, c.want)
		}
	}
}

func TestExpandFailsClosedOnMissing(t *testing.T) {
	sc := mkScope(t, `{"a": {"b": 1}}`)
	if _, err := sc.expand("x=${a.missing}"); err == nil {
		t.Fatal("expand must fail on a missing reference (fail closed)")
	}
	if _, err := sc.expand("x=${a.b"); err == nil {
		t.Fatal("expand must fail on an unterminated reference")
	}
}

func TestSingleRef(t *testing.T) {
	if r, ok := singleRef("${item}"); !ok || r != "item" {
		t.Errorf("singleRef(${item}) = %q,%v", r, ok)
	}
	if r, ok := singleRef("  ${item.pr}  "); !ok || r != "item.pr" {
		t.Errorf("singleRef trimmed = %q,%v", r, ok)
	}
	if _, ok := singleRef("a ${item}"); ok {
		t.Error("singleRef with surrounding text should be false")
	}
	if _, ok := singleRef("${a}${b}"); ok {
		t.Error("singleRef with two refs should be false")
	}
}

func TestTemplateRoots(t *testing.T) {
	roots := templateRoots("hi ${a.b} and ${c} and ${a.x}")
	want := map[string]bool{"a": true, "c": true}
	if len(roots) != 2 {
		t.Fatalf("roots = %v", roots)
	}
	for _, r := range roots {
		if !want[r] {
			t.Errorf("unexpected root %q", r)
		}
	}
}
