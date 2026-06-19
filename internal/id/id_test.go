package id

import (
	"strings"
	"testing"
)

func TestNewAndToken(t *testing.T) {
	a := New("usr")
	b := New("usr")
	if a == b {
		t.Fatal("New must produce unique ids")
	}
	if !strings.HasPrefix(a, "usr_") {
		t.Fatalf("New = %q, want usr_ prefix", a)
	}
	if got := len(a); got != len("usr_")+32 { // 16 random bytes -> 32 hex chars
		t.Fatalf("New length = %d, want %d", got, len("usr_")+32)
	}

	tok := Token(32)
	if len(tok) != 64 { // 32 bytes -> 64 hex chars
		t.Fatalf("Token(32) length = %d, want 64", len(tok))
	}
	if Token(32) == tok {
		t.Fatal("Token must produce unique secrets")
	}
}
