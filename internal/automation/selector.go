package automation

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The selector engine resolves a *reference* — `<step_id>.<path>` — against a scope
// of prior step outputs (plus the for_each bindings `item`/`index`). It is the v1
// surface settled in research-findings.md C4: dot/bracket paths only (a.b, a.items[0].pr),
// NO wildcards/filters/recursion, and depth-/length-bounded against abuse. A
// *template* is a string interpolating `${...}` references; a missing reference is a
// hard error (fail closed), never a silent empty string.

// scope holds the values a reference can resolve against: each completed step's
// output keyed by step id, overlaid with the current loop's `item`/`index`.
type scope map[string]any

// pathSeg is one resolved path component: a map key, or a slice index (Index>=0).
type pathSeg struct {
	key   string
	index int
	isIdx bool
}

// parsePath splits a reference like `find.items[0].pr` into segments. It rejects an
// empty reference, an over-long one, malformed brackets, and an over-deep path.
func parsePath(ref string) ([]pathSeg, error) {
	if ref == "" {
		return nil, fmt.Errorf("empty reference")
	}
	if len(ref) > maxSelectorLen {
		return nil, fmt.Errorf("reference is too long")
	}
	var segs []pathSeg
	i := 0
	expectKey := true // a path starts with a key, and after a '.' another key
	for i < len(ref) {
		switch ref[i] {
		case '.':
			if expectKey {
				return nil, fmt.Errorf("reference %q has an empty path segment", ref)
			}
			expectKey = true
			i++
		case '[':
			j := strings.IndexByte(ref[i:], ']')
			if j <= 1 { // need at least one digit between [ ]
				return nil, fmt.Errorf("reference %q has a malformed index", ref)
			}
			num := ref[i+1 : i+j]
			idx, err := strconv.Atoi(num)
			if err != nil || idx < 0 {
				return nil, fmt.Errorf("reference %q has a non-numeric or negative index", ref)
			}
			segs = append(segs, pathSeg{index: idx, isIdx: true})
			i += j + 1
			expectKey = false
		default:
			// Read an identifier up to the next '.' or '['.
			start := i
			for i < len(ref) && ref[i] != '.' && ref[i] != '[' {
				i++
			}
			key := ref[start:i]
			if key == "" {
				return nil, fmt.Errorf("reference %q has an empty key", ref)
			}
			segs = append(segs, pathSeg{key: key})
			expectKey = false
		}
		if len(segs) > maxSelectorDepth {
			return nil, fmt.Errorf("reference %q is too deeply nested", ref)
		}
	}
	if expectKey {
		return nil, fmt.Errorf("reference %q ends with a trailing '.'", ref)
	}
	if len(segs) == 0 || segs[0].isIdx {
		return nil, fmt.Errorf("reference %q must start with a step id", ref)
	}
	return segs, nil
}

// rootID returns the leading step id of a reference (the part before the first '.'
// or '['), used by validation to check the reference targets a known step or loop
// variable. It returns "" for a malformed reference.
func rootID(ref string) string {
	segs, err := parsePath(ref)
	if err != nil || len(segs) == 0 {
		return ""
	}
	return segs[0].key
}

// errRefNotFound distinguishes "the path doesn't exist" (likely an authoring bug —
// fail closed) from a structural error. Both are returned as errors; this lets the
// engine word the message helpfully.
type errRefNotFound struct{ ref string }

func (e errRefNotFound) Error() string { return "reference " + e.ref + " did not resolve to a value" }

// resolve walks a reference against sc, returning the value. A missing key/index is
// errRefNotFound (fail closed); a type mismatch (indexing a map, keying a slice) is
// a structural error.
func (sc scope) resolve(ref string) (any, error) {
	segs, err := parsePath(ref)
	if err != nil {
		return nil, err
	}
	cur, ok := sc[segs[0].key]
	if !ok {
		return nil, errRefNotFound{ref}
	}
	for _, seg := range segs[1:] {
		switch node := cur.(type) {
		case map[string]any:
			if seg.isIdx {
				return nil, fmt.Errorf("reference %q indexes an object", ref)
			}
			v, ok := node[seg.key]
			if !ok {
				return nil, errRefNotFound{ref}
			}
			cur = v
		case []any:
			if !seg.isIdx {
				return nil, fmt.Errorf("reference %q keys an array", ref)
			}
			if seg.index >= len(node) {
				return nil, errRefNotFound{ref}
			}
			cur = node[seg.index]
		default:
			return nil, fmt.Errorf("reference %q descends past a scalar value", ref)
		}
	}
	return cur, nil
}

// renderValue renders a resolved value for template interpolation: a string as
// itself, a number/bool as its literal, null as "null", and an object/array as
// compact JSON.
func renderValue(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "null", nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case json.Number:
		return t.String(), nil
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("value could not be rendered")
		}
		return string(b), nil
	}
}

// expand interpolates every ${...} reference in tmpl against sc. A reference that
// fails to resolve aborts with an error (fail closed). The number of expansions and
// the total output length are bounded.
func (sc scope) expand(tmpl string) (string, error) {
	if len(tmpl) > maxSelectorLen*maxTemplateExpand {
		return "", fmt.Errorf("template is too long")
	}
	var b strings.Builder
	n := 0
	for {
		start := strings.Index(tmpl, "${")
		if start < 0 {
			b.WriteString(tmpl)
			break
		}
		end := strings.Index(tmpl[start:], "}")
		if end < 0 {
			return "", fmt.Errorf("template has an unterminated ${ reference")
		}
		end += start
		b.WriteString(tmpl[:start])
		ref := strings.TrimSpace(tmpl[start+2 : end])
		v, err := sc.resolve(ref)
		if err != nil {
			return "", err
		}
		rendered, err := renderValue(v)
		if err != nil {
			return "", err
		}
		b.WriteString(rendered)
		if b.Len() > maxExpandedBytes {
			return "", fmt.Errorf("template expanded past the size limit")
		}
		if n++; n > maxTemplateExpand {
			return "", fmt.Errorf("template has too many ${} expansions")
		}
		tmpl = tmpl[end+1:]
	}
	if b.Len() > maxExpandedBytes {
		return "", fmt.Errorf("template expanded past the size limit")
	}
	return b.String(), nil
}

// scanTemplateRefs walks every ${...} reference in tmpl STRICTLY, returning each well-formed
// reference's deduplicated root id — or an error for an UNTERMINATED ${ or a MALFORMED
// reference whose root can't be parsed. These are exactly the conditions the runtime expander
// fails closed on, so validation (which calls this) rejects a broken template BEFORE enable
// rather than after an earlier side-effecting step has already run.
func scanTemplateRefs(tmpl string) ([]string, error) {
	var roots []string
	seen := map[string]bool{}
	rest := tmpl
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			break
		}
		end := strings.Index(rest[start:], "}")
		if end < 0 {
			return roots, fmt.Errorf("template has an unterminated ${ reference")
		}
		end += start
		ref := strings.TrimSpace(rest[start+2 : end])
		root := rootID(ref)
		if root == "" {
			return roots, fmt.Errorf("template has a malformed reference %q", "${"+ref+"}")
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
		rest = rest[end+1:]
	}
	return roots, nil
}

// templateRoots returns the set of leading identifiers every ${...} reference in
// tmpl targets, so validation can confirm each names a known step or loop variable.
func templateRoots(tmpl string) []string {
	var roots []string
	seen := map[string]bool{}
	rest := tmpl
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			break
		}
		end := strings.Index(rest[start:], "}")
		if end < 0 {
			break
		}
		end += start
		ref := strings.TrimSpace(rest[start+2 : end])
		if r := rootID(ref); r != "" && !seen[r] {
			seen[r] = true
			roots = append(roots, r)
		}
		rest = rest[end+1:]
	}
	return roots
}
