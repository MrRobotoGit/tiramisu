package auth

import (
	"strings"
	"testing"
)

func TestNewTokenFormat(t *testing.T) {
	a, b := newToken(), newToken()
	if !strings.HasPrefix(a, "tmr_") || len(a) != 4+43 || a == b {
		t.Fatalf("token %q", a)
	}
	if tokenPrefix(a) != a[:8] {
		t.Fatal("prefix")
	}
	if hashSecret(a) == hashSecret(b) || len(hashSecret(a)) != 64 {
		t.Fatal("hash")
	}
}

func TestPassword(t *testing.T) {
	h, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(h, "correct horse") || checkPassword(h, "wrong") {
		t.Fatal("check")
	}
	if _, err := hashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if _, err := hashPassword(strings.Repeat("x", 73)); err == nil {
		t.Fatal("password over 72 bytes accepted")
	}
}
