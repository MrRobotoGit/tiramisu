package auth

import (
	"sync"

	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	tokenPrefixLiteral = "tmr_"
	minPassword        = 10
)

// bcryptCost is a var so tests can lower it; production keeps 12.
var bcryptCost = 12

var (
	ErrWeakPassword = errors.New("Password must be at least 10 characters.")
	ErrLongPassword = errors.New("Password is too long.") // bcrypt reads at most 72 bytes
)

func randomString() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newToken() string              { return tokenPrefixLiteral + randomString() }
func tokenPrefix(tok string) string { return tok[:8] }

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hashPassword(pw string) (string, error) {
	if utf8.RuneCountInString(pw) < minPassword {
		return "", ErrWeakPassword
	}
	if len(pw) > 72 {
		return "", ErrLongPassword
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// dummyHash keeps a login without a user as slow as a wrong-password one.
var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("tiramisu-dummy-password"), bcryptCost)
	return h
})
