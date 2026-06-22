package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// envNameRe validates an injected environment-variable name.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvGrant binds a vaulted secret to the environment-variable name it is injected
// as for one run. Grants live in the user's Sandbox Environment policy; the value
// is resolved (decrypted) only at run time and only into the env of that one run.
type EnvGrant struct {
	SecretID string
	EnvName  string
}

// Injection is the resolved, run-scoped materialization of granted secrets. It
// exposes the env pairs to hand to the sandbox and a Redactor built from the
// plaintext values so the same values can be scrubbed from captured output and
// audit logs. It holds plaintext in memory only for the lifetime of a run.
type Injection struct {
	env      map[string]string
	redactor *Redactor
}

// EnvPairs returns the injected variables as "NAME=VALUE" strings, sorted for
// deterministic ordering (stable tests + reproducible argv).
func (in *Injection) EnvPairs() []string {
	out := make([]string, 0, len(in.env))
	for k, v := range in.env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Redactor returns a redactor seeded with every injected secret value.
func (in *Injection) Redactor() *Redactor { return in.redactor }

// AllValuesRedactor builds a redactor over EVERY one of the user's currently
// vaulted secret values — not just the granted ones. Output/audit redaction uses
// this (decoupled from injection): a secret written into the durable workspace
// while granted must stay scrubbed from a later tool's output even after its grant
// is removed, as long as the secret still exists in the vault. Server-internal only.
func (v *Vault) AllValuesRedactor(ctx context.Context, userID string) (*Redactor, error) {
	metas, err := v.store.ListSecretsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	var values []string
	for _, m := range metas {
		val, err := v.Get(ctx, userID, m.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue // deleted between the list and the read
		}
		if err != nil {
			return nil, err
		}
		values = append(values, val)
	}
	return NewRedactor(values), nil
}

// Materialize resolves the granted secrets for userID into a run-scoped Injection.
// A grant whose secret was deleted is skipped (so removing a secret never breaks
// every run that still references it); a grant with a malformed env name is an
// error the user should fix. The resolved plaintext lives only in the returned
// Injection — never logged, never persisted.
func (v *Vault) Materialize(ctx context.Context, userID string, grants []EnvGrant) (*Injection, error) {
	env := make(map[string]string, len(grants))
	var values []string
	for _, g := range grants {
		if !envNameRe.MatchString(g.EnvName) {
			return nil, fmt.Errorf("vault: invalid env name %q for injected secret", g.EnvName)
		}
		val, err := v.Get(ctx, userID, g.SecretID)
		if errors.Is(err, store.ErrNotFound) {
			continue // grant points at a deleted secret — skip it
		}
		if err != nil {
			return nil, err
		}
		env[g.EnvName] = val
		if val != "" {
			values = append(values, val)
		}
	}
	return &Injection{env: env, redactor: NewRedactor(values)}, nil
}

// Redactor scrubs known secret values from arbitrary text so a secret a tool
// echoes to stdout/stderr (or a value that lands in an audit summary) never
// surfaces in logs, the admin UI, or a model-visible tool result.
type Redactor struct {
	values []string // non-empty, sorted longest-first
}

// redactMark replaces a matched secret value.
const redactMark = "[redacted]"

// NewRedactor builds a redactor over the given secret values. Empty values are
// dropped; remaining values are sorted longest-first so an overlapping shorter
// value can't leave a fragment of a longer one behind.
func NewRedactor(values []string) *Redactor {
	seen := make(map[string]struct{}, len(values))
	var vals []string
	for _, val := range values {
		if val == "" {
			continue
		}
		if _, dup := seen[val]; dup {
			continue
		}
		seen[val] = struct{}{}
		vals = append(vals, val)
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	return &Redactor{values: vals}
}

// RedactJSON scrubs secrets from JSON data by DECODING it and redacting the plaintext
// string values (then re-marshaling), so ANY valid JSON encoding of a secret — `\"`,
// `\/`, `\uXXXX`, etc. — is normalized to the plaintext the redactor knows and matched
// (enumerating one canonical escaped form is not enough). Keys are redacted too. If
// data is not valid JSON it falls back to a raw byte redaction (best-effort).
func (r *Redactor) RedactJSON(data []byte) []byte {
	if r == nil {
		return data
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // numbers stay json.Number (their string form), so a numeric secret is seen
	var v any
	if err := dec.Decode(&v); err != nil {
		return r.RedactBytes(data)
	}
	out, err := json.Marshal(r.redactJSONValue(v))
	if err != nil {
		return r.RedactBytes(data)
	}
	return out
}

func (r *Redactor) redactJSONValue(v any) any {
	switch t := v.(type) {
	case string:
		// RedactText (not Redact): a decoded string value can ITSELF carry a secret in its
		// JSON-escaped form (a prior tool that printed json.Marshal of a credential), which a
		// plaintext-only scrub would leave intact.
		return r.RedactText(t)
	case json.Number:
		// A numeric secret (e.g. 31337) decodes to json.Number, whose string form holds
		// the digits. If it carries a secret, return the redacted STRING (it can no longer
		// marshal as a number); otherwise keep the json.Number to preserve precision.
		if red := r.Redact(string(t)); red != string(t) {
			return red
		}
		return t
	case []any:
		for i := range t {
			t[i] = r.redactJSONValue(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[r.RedactText(k)] = r.redactJSONValue(val)
		}
		return out
	default:
		return v
	}
}

// JSONContainsSecret reports whether JSON data carries a secret. It checks the raw bytes
// (catching a numeric/scalar secret, which appears literally, and a plaintext secret in
// non-JSON data) AND the DECODED string/number values (catching a secret embedded under
// any valid JSON escaping). Either path matching is a hit.
func (r *Redactor) JSONContainsSecret(data []byte) bool {
	if r == nil {
		return false
	}
	if r.ContainsSecret(string(data)) {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false // not JSON — the raw check above already covered it
	}
	return r.jsonValueContainsSecret(v)
}

func (r *Redactor) jsonValueContainsSecret(v any) bool {
	switch t := v.(type) {
	case string:
		// ContainsSecretText (not ContainsSecret): a decoded string value can itself carry a
		// secret in its JSON-escaped form, which a plaintext-only check would miss.
		return r.ContainsSecretText(t)
	case json.Number:
		return r.ContainsSecret(string(t))
	case []any:
		for _, e := range t {
			if r.jsonValueContainsSecret(e) {
				return true
			}
		}
		return false
	case map[string]any:
		for k, val := range t {
			if r.ContainsSecretText(k) || r.jsonValueContainsSecret(val) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// Merge returns a redactor that scrubs the UNION of this redactor's values and
// other's. Used so output stays redacted for a secret value even if its grant was
// revoked between when a call was raised and when it runs (the revoked value is
// still known and scrubbed, even though it is no longer injected).
func (r *Redactor) Merge(other *Redactor) *Redactor {
	var vals []string
	if r != nil {
		vals = append(vals, r.values...)
	}
	if other != nil {
		vals = append(vals, other.values...)
	}
	return NewRedactor(vals)
}

// MaxValueLen returns the length of the longest secret value in EITHER its plaintext or its
// JSON-string-escaped form (0 if none). Callers truncating output use it as a safe margin: a
// secret split across a truncation boundary leaves at most MaxValueLen-1 bytes that exact-match
// redaction can't catch — and since RedactBytes/RedactText also scrub the (longer) escaped
// form, the margin must cover that length too.
func (r *Redactor) MaxValueLen() int {
	if r == nil {
		return 0
	}
	max := 0
	for _, v := range r.values {
		if len(v) > max {
			max = len(v)
		}
		if b, err := json.Marshal(v); err == nil && len(b)-2 > max {
			max = len(b) - 2 // the escaped body length, sans the surrounding quotes
		}
	}
	return max
}

// Redact replaces every occurrence of every known secret value with a placeholder.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	for _, val := range r.values {
		s = strings.ReplaceAll(s, val, redactMark)
	}
	return s
}

// RedactText scrubs secret values from non-JSON text in BOTH their plaintext and
// JSON-string-escaped (json.Marshal'd body) forms — so a credential rendered into an audit /
// event summary via a json.Marshal'd object value (its `\n`/`\"`/`\\` escaping) is scrubbed,
// not just a plaintext occurrence. It is the redaction counterpart of ContainsSecretText; use
// it for any human-/log-facing summary that may embed an escaped secret.
func (r *Redactor) RedactText(s string) string {
	if r == nil {
		return s
	}
	s = r.Redact(s) // plaintext forms (longest-first)
	for _, v := range r.values {
		if b, err := json.Marshal(v); err == nil && len(b) >= 2 {
			if esc := string(b[1 : len(b)-1]); esc != v {
				s = strings.ReplaceAll(s, esc, redactMark)
			}
		}
	}
	return s
}

// RedactBytes is the escaped-aware []byte form for captured output buffers: it scrubs both
// the plaintext AND JSON-string-escaped form of every secret, so a tool/agent that echoes
// json.Marshal(secret) can't persist the escaped credential into a capture file / run record /
// artifact (which a plaintext-only scrub would leave intact).
func (r *Redactor) RedactBytes(b []byte) []byte {
	if r == nil || len(r.values) == 0 {
		return b
	}
	return []byte(r.RedactText(string(b)))
}

// ContainsSecret reports whether s contains any known secret value. Callers that
// must decide "does this carry a secret?" use this rather than comparing s to its
// redacted form — a secret whose value equals the redaction marker would make the
// post-replacement string identical and slip a string-equality check.
func (r *Redactor) ContainsSecret(s string) bool {
	if r == nil {
		return false
	}
	for _, v := range r.values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// ContainsSecretText reports whether s carries a secret value either as PLAINTEXT or in its
// JSON-string-escaped form. It catches a secret that reached a non-JSON outbound string (an
// llm prompt, an agent argv) by way of a `json.Marshal`'d object/array value — whose `\n`/
// `\"`/`\\` escaping a plaintext search would miss. For a payload that is itself JSON (a
// schema, a marshaled inputs array), prefer JSONContainsSecret, which decodes ANY encoding.
func (r *Redactor) ContainsSecretText(s string) bool {
	if r == nil {
		return false
	}
	if r.ContainsSecret(s) {
		return true
	}
	for _, v := range r.values {
		// json.Marshal escapes a value the SAME way template-expanding an object/array into the
		// prompt does, so the escaped body it produces is exactly what would appear in s.
		if b, err := json.Marshal(v); err == nil && len(b) >= 2 {
			if esc := string(b[1 : len(b)-1]); esc != v && strings.Contains(s, esc) {
				return true
			}
		}
	}
	return false
}
