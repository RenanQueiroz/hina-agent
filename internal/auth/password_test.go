package auth

import "testing"

func TestPasswordHashVerify(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h == pw {
		t.Fatal("hash must not equal plaintext")
	}
	ok, err := VerifyPassword(pw, h)
	if err != nil || !ok {
		t.Fatalf("verify correct password: ok=%v err=%v", ok, err)
	}
	bad, err := VerifyPassword("wrong", h)
	if err != nil || bad {
		t.Fatalf("verify wrong password: bad=%v err=%v", bad, err)
	}
}
