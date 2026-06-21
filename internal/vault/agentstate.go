package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// Agent-state is per-user, per-provider credential material for a callable coding
// agent (Phase 8): the tar of a CLI's credential store (CODEX_HOME / CLAUDE_CONFIG_DIR
// / Cursor state) for a browser/subscription profile, or the raw API key/OAuth token
// for a key profile. It is treated as exactly the same secret material as a vaulted
// secret — envelope-encrypted, an owner-private file on disk, NEVER in the database,
// and never surfaced in the admin UI/logs (only a coarse profile status is). The
// blob is opaque to the vault; the AgentProfile row (internal/store) records HOW to
// interpret it (auth type) without storing the credential.

// agentStatePath resolves the encrypted agent-state blob path for (userID,
// provider), rejecting unsafe components (defense in depth — both are validated/
// server-issued upstream).
func (v *Vault) agentStatePath(userID, provider string) (string, error) {
	if !safeComponent.MatchString(userID) || !safeComponent.MatchString(provider) {
		return "", fmt.Errorf("vault: unsafe agent-state component")
	}
	return filepath.Join(v.root, userID, "agents", provider+".enc"), nil
}

// PutAgentState stores (or replaces) a provider's agent-state blob for a user,
// envelope-encrypted and written atomically with owner-only permissions.
func (v *Vault) PutAgentState(userID, provider string, data []byte) error {
	path, err := v.agentStatePath(userID, provider)
	if err != nil {
		return err
	}
	return v.writeEnvelope(path, data)
}

// GetAgentState decrypts and returns a provider's agent-state blob, or
// store.ErrNotFound when none exists. Server-internal only — never an API response.
func (v *Vault) GetAgentState(userID, provider string) ([]byte, error) {
	path, err := v.agentStatePath(userID, provider)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("vault: read agent state: %w", err)
	}
	return v.openEnvelope(raw)
}

// GetAgentStateVersioned decrypts a provider's agent-state blob AND returns a
// WRITE-UNIQUE version of it, from a SINGLE file read. The version hashes the raw
// ENCRYPTED envelope, whose per-write random data key + nonce make the ciphertext unique
// even when the plaintext is unchanged — so a delete+recreate of the SAME credential
// value yields a DIFFERENT version (a plaintext content hash would not). The callable-
// agent launch/persist fences use this so a same-value re-auth during a run is detected.
func (v *Vault) GetAgentStateVersioned(userID, provider string) ([]byte, string, error) {
	path, err := v.agentStatePath(userID, provider)
	if err != nil {
		return nil, "", err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "", store.ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("vault: read agent state: %w", err)
	}
	data, err := v.openEnvelope(raw)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(raw)
	return data, hex.EncodeToString(h[:]), nil
}

// AgentStateVersion returns the WRITE-UNIQUE version of a provider's agent-state blob
// (the raw encrypted-envelope hash), or "" if there is none/unreadable.
func (v *Vault) AgentStateVersion(userID, provider string) string {
	path, err := v.agentStatePath(userID, provider)
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// HasAgentState reports whether a provider's agent-state blob exists for a user
// (without decrypting it).
func (v *Vault) HasAgentState(userID, provider string) bool {
	path, err := v.agentStatePath(userID, provider)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// DeleteAgentState removes a provider's agent-state blob (logout). Tolerant of an
// already-absent blob so a partial delete is recoverable.
func (v *Vault) DeleteAgentState(userID, provider string) error {
	path, err := v.agentStatePath(userID, provider)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("vault: remove agent state: %w", err)
	}
	return nil
}
