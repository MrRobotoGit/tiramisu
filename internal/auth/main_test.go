package auth

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	bcryptCost = bcrypt.MinCost // cost 12 under -race takes seconds per hash
	os.Exit(m.Run())
}
