// Package vault is Hina's per-user secret store. It uses envelope encryption: a
// fresh random data-encryption key (DEK) encrypts each secret value, and the DEK
// is wrapped by the local master key (OS keyring/DPAPI/ACL-guarded file via
// internal/platform). The encrypted envelope is written as an owner-private file
// on disk — NOT in the database — so a database compromise or the normal admin UI
// reveals only metadata (name/description), never the value or even its
// ciphertext. The boundary it does NOT defend against is a malicious host/root
// admin or a modified server binary (research-findings C5): unattended Automations
// require the running server to decrypt granted secrets, so the server process can
// always reach plaintext.
//
// Threat model in one line: protects DB-dump + admin-UI snooping; does not protect
// against code running as the Hina server.
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// dekLen is the per-secret data-encryption-key length (AES-256).
const dekLen = 32

// envelopeVersion tags the on-disk format so it can evolve.
const envelopeVersion = 1

// safeComponent guards the path components used to build a secret's blob path.
// User and secret ids are server-issued (internal/id: prefix_hex), so this never
// rejects a legitimate value; it fails closed if a hostile id ever reaches here.
var safeComponent = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Vault encrypts/decrypts per-user secrets and tracks their metadata in the store.
type Vault struct {
	master cipher.AEAD // wraps/unwraps per-secret DEKs with the master key
	root   string      // owner-private directory holding <userID>/<secretID>.enc
	store  *store.Store
}

// envelope is the on-disk encrypted record. The JSON []byte fields are
// base64-encoded; none of them is meaningful without the master key.
type envelope struct {
	Version    int    `json:"v"`
	WrappedDEK []byte `json:"wdek"` // master-key-GCM(dek)
	DEKNonce   []byte `json:"dn"`   // nonce for WrappedDEK
	ValueNonce []byte `json:"vn"`   // nonce for Value
	Value      []byte `json:"val"`  // dek-GCM(plaintext)
}

// New builds a Vault. masterKey must be platform.MasterKeyLen bytes; root is made
// owner-private (0700) so no other local principal can read the encrypted blobs.
func New(masterKey []byte, root string, st *store.Store) (*Vault, error) {
	if len(masterKey) != platform.MasterKeyLen {
		return nil, fmt.Errorf("vault: master key must be %d bytes, got %d", platform.MasterKeyLen, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("vault: aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: gcm: %w", err)
	}
	if err := platform.EnsurePrivateDir(root); err != nil {
		return nil, fmt.Errorf("vault: secure root: %w", err)
	}
	return &Vault{master: aead, root: root, store: st}, nil
}

// Put stores a new secret value under name and returns its metadata. The metadata
// row is inserted first (its unique (user,name) index rejects duplicates as
// store.ErrConflict); the encrypted blob is written only after, and the metadata
// row is rolled back if the blob can't be written — so a half-created secret never
// lingers.
func (v *Vault) Put(ctx context.Context, userID, name, description, value string) (store.SecretMeta, error) {
	if name == "" {
		return store.SecretMeta{}, errors.New("vault: secret name is empty")
	}
	meta := store.SecretMeta{ID: id.New("sec"), UserID: userID, Name: name, Description: description}
	if err := v.store.CreateSecretMeta(ctx, meta); err != nil {
		return store.SecretMeta{}, err
	}
	if err := v.writeBlob(userID, meta.ID, value); err != nil {
		// Roll back the metadata so the name is free again and no dangling row remains.
		_ = v.store.DeleteSecretMeta(ctx, userID, meta.ID)
		return store.SecretMeta{}, err
	}
	got, err := v.store.GetSecretMeta(ctx, userID, meta.ID)
	if err != nil {
		return store.SecretMeta{}, err
	}
	return got, nil
}

// Get decrypts and returns a secret's plaintext value. Server-internal only —
// used to inject secrets into a run; never wired to an API response.
func (v *Vault) Get(ctx context.Context, userID, secretID string) (string, error) {
	// Confirm ownership via the metadata row before touching the blob, so a caller
	// can't read another user's secret even by guessing an id.
	if _, err := v.store.GetSecretMeta(ctx, userID, secretID); err != nil {
		return "", err
	}
	path, err := v.blobPath(userID, secretID)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("vault: read blob: %w", err)
	}
	plain, err := v.openEnvelope(raw)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// openEnvelope decrypts an on-disk envelope: it unwraps the per-blob DEK with the
// master key, then decrypts the value with the DEK. Shared by secrets and
// agent-state so the crypto lives in exactly one place.
func (v *Vault) openEnvelope(raw []byte) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("vault: decode blob: %w", err)
	}
	if env.Version != envelopeVersion {
		return nil, fmt.Errorf("vault: unsupported envelope version %d", env.Version)
	}
	dek, err := v.master.Open(nil, env.DEKNonce, env.WrappedDEK, nil)
	if err != nil {
		return nil, fmt.Errorf("vault: unwrap dek: %w", err)
	}
	plain, err := openWith(dek, env.ValueNonce, env.Value)
	if err != nil {
		return nil, fmt.Errorf("vault: decrypt value: %w", err)
	}
	return plain, nil
}

// List returns a user's secret metadata (no values).
func (v *Vault) List(ctx context.Context, userID string) ([]store.SecretMeta, error) {
	return v.store.ListSecretsByUser(ctx, userID)
}

// UpdateDescription edits a secret's description.
func (v *Vault) UpdateDescription(ctx context.Context, userID, secretID, description string) error {
	return v.store.UpdateSecretMeta(ctx, userID, secretID, description)
}

// Delete removes a secret's encrypted blob and then its metadata row. The blob is
// removed FIRST (after an ownership check) so a failure can never leave the
// metadata gone while the encrypted blob lingers on disk with no cleanup path: if
// blob removal fails, the metadata is untouched and a retry works; if metadata
// removal fails after the blob is gone, a retry re-removes the (already-absent)
// blob and clears the row. Tolerant of a missing blob so a partial delete is
// recoverable.
func (v *Vault) Delete(ctx context.Context, userID, secretID string) error {
	// Confirm ownership before touching the blob (DeleteSecretMeta is also scoped,
	// but the blob removal happens first now).
	if _, err := v.store.GetSecretMeta(ctx, userID, secretID); err != nil {
		return err
	}
	path, err := v.blobPath(userID, secretID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("vault: remove blob: %w", err)
	}
	return v.store.DeleteSecretMeta(ctx, userID, secretID)
}

// writeBlob encrypts value and writes its envelope atomically with owner-only
// permissions to the secret's blob path.
func (v *Vault) writeBlob(userID, secretID, value string) error {
	path, err := v.blobPath(userID, secretID)
	if err != nil {
		return err
	}
	return v.writeEnvelope(path, []byte(value))
}

// sealEnvelope envelope-encrypts plaintext: a fresh random DEK encrypts the value,
// and the DEK is wrapped by the master key. Shared by secrets and agent-state.
func (v *Vault) sealEnvelope(plaintext []byte) ([]byte, error) {
	dek := make([]byte, dekLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("vault: generate dek: %w", err)
	}
	valNonce, valCT, err := sealWith(dek, plaintext)
	if err != nil {
		return nil, err
	}
	dekNonce := make([]byte, v.master.NonceSize())
	if _, err := rand.Read(dekNonce); err != nil {
		return nil, fmt.Errorf("vault: dek nonce: %w", err)
	}
	wrapped := v.master.Seal(nil, dekNonce, dek, nil)
	raw, err := json.Marshal(envelope{
		Version:    envelopeVersion,
		WrappedDEK: wrapped,
		DEKNonce:   dekNonce,
		ValueNonce: valNonce,
		Value:      valCT,
	})
	if err != nil {
		return nil, fmt.Errorf("vault: encode blob: %w", err)
	}
	return raw, nil
}

// writeEnvelope seals plaintext and writes it atomically (temp + rename) with
// owner-only permissions to path, creating the parent dir owner-private.
func (v *Vault) writeEnvelope(path string, plaintext []byte) error {
	raw, err := v.sealEnvelope(plaintext)
	if err != nil {
		return err
	}
	if err := platform.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("vault: secure user dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("vault: write blob: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vault: commit blob: %w", err)
	}
	return nil
}

// blobPath resolves the encrypted-blob path for a secret, rejecting unsafe id
// components (defense in depth — ids are server-issued and always safe).
func (v *Vault) blobPath(userID, secretID string) (string, error) {
	if !safeComponent.MatchString(userID) || !safeComponent.MatchString(secretID) {
		return "", fmt.Errorf("vault: unsafe id component")
	}
	return filepath.Join(v.root, userID, secretID+".enc"), nil
}

// sealWith encrypts plaintext under a 32-byte key with AES-256-GCM, returning the
// fresh nonce and ciphertext.
func sealWith(key, plaintext []byte) (nonce, ciphertext []byte, err error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("vault: nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, plaintext, nil), nil
}

// openWith decrypts ciphertext produced by sealWith.
func openWith(key, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: aes cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
