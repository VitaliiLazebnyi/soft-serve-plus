package backend

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashPassword(t *testing.T) {
	hash, err := HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("hash is empty")
	}
}

func TestVerifyPassword(t *testing.T) {
	hash, err := HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("password", hash) {
		t.Fatal("password did not verify")
	}
}

// An empty hash, for a missing user or one without a password, never
// verifies, but is compared against a dummy hash of the same cost so it
// takes as long as a real password check.
func TestVerifyPasswordEmptyHash(t *testing.T) {
	for _, password := range []string{"", "password"} {
		if VerifyPassword(password, "") {
			t.Fatalf("password %q verified against an empty hash", password)
		}
	}
	real, err := HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	want, err := bcrypt.Cost([]byte(real))
	if err != nil {
		t.Fatal(err)
	}
	got, err := bcrypt.Cost(dummyHash())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("dummy hash cost = %d, want %d", got, want)
	}
}

func TestGenerateToken(t *testing.T) {
	token := GenerateToken()
	if token == "" {
		t.Fatal("token is empty")
	}
}

func TestHashToken(t *testing.T) {
	token := GenerateToken()
	hash := HashToken(token)
	if hash == "" {
		t.Fatal("hash is empty")
	}
}
