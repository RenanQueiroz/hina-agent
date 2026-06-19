package auth

import (
	"context"
	"errors"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// ErrInvalidCredentials is returned by Authenticate on a bad username/password.
var ErrInvalidCredentials = errors.New("invalid credentials")

// BootstrapResult reports a first-run admin credential. Password is the one-time
// plaintext (printed once); set only when Created is true.
type BootstrapResult struct {
	Created  bool
	Username string
	Password string
}

// EnsureAdmin creates a bootstrap admin (random password, must-change) if no
// admin exists. The plaintext is returned exactly once and never stored.
func EnsureAdmin(ctx context.Context, st *store.Store) (BootstrapResult, error) {
	n, err := st.CountByRole(ctx, "admin")
	if err != nil {
		return BootstrapResult{}, err
	}
	if n > 0 {
		return BootstrapResult{Created: false}, nil
	}

	password := id.Token(12) // 24 hex chars
	hash, err := HashPassword(password)
	if err != nil {
		return BootstrapResult{}, err
	}
	u := store.User{
		ID:                 id.New("usr"),
		Username:           "admin",
		Role:               "admin",
		PasswordHash:       hash,
		MustChangePassword: true,
	}
	if err := st.CreateUser(ctx, u); err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{Created: true, Username: "admin", Password: password}, nil
}

// Authenticate verifies a username/password and returns the user.
func Authenticate(ctx context.Context, st *store.Store, username, password string) (store.User, error) {
	u, err := st.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrInvalidCredentials
		}
		return store.User{}, err
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil || !ok {
		return store.User{}, ErrInvalidCredentials
	}
	return u, nil
}

// ChangePassword sets a new password for a user (clears must_change_password)
// and revokes all of the user's existing login sessions, so a credential issued
// under the old password cannot outlive the change. Callers that want the
// current request to stay logged in must reissue a session afterward.
func ChangePassword(ctx context.Context, st *store.Store, userID, newPassword string) error {
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return st.UpdateUserPasswordAndRevokeSessions(ctx, userID, hash)
}

// LANAllowed reports whether LAN binding is permitted — i.e. no admin still
// holds the bootstrap must-change-password flag.
func LANAllowed(ctx context.Context, st *store.Store) (bool, error) {
	n, err := st.CountAdminsRequiringPasswordChange(ctx)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}
