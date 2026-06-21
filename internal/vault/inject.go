package vault

import (
	"context"
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

// MaxValueLen returns the length of the longest secret value (0 if none). Callers
// truncating output use it as a safe margin: a secret split across a truncation
// boundary leaves at most MaxValueLen-1 bytes that exact-match redaction can't catch.
func (r *Redactor) MaxValueLen() int {
	if r == nil {
		return 0
	}
	max := 0
	for _, v := range r.values {
		if len(v) > max {
			max = len(v)
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

// RedactBytes is the []byte form of Redact (for captured output buffers).
func (r *Redactor) RedactBytes(b []byte) []byte {
	if r == nil || len(r.values) == 0 {
		return b
	}
	return []byte(r.Redact(string(b)))
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
