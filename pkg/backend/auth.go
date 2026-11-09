package backend

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"charm.land/log/v2"
	"golang.org/x/crypto/bcrypt"
)

const saltySalt = "salty-soft-serve"

// HashPassword hashes the password using bcrypt.
func HashPassword(password string) (string, error) {
	crypt, err := bcrypt.GenerateFromPassword([]byte(password+saltySalt), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}

	return string(crypt), nil
}

// dummyHash is a password hash no password matches. VerifyPassword compares
// against it when there is no hash, so a missing user or a user without a
// password costs the same bcrypt work as a wrong password.
var dummyHash = sync.OnceValue(func() []byte {
	hash, err := HashPassword(GenerateToken())
	if err != nil {
		// Unreachable: bcrypt fails only on passwords over 72 bytes.
		panic(err)
	}
	return []byte(hash)
})

// VerifyPassword verifies the password against the hash. An empty hash, for
// a missing user or one without a password, never verifies but takes as long
// as a real comparison, so the response time does not tell the two apart.
func VerifyPassword(password, hash string) bool {
	if hash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(password+saltySalt))
		return false
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password+saltySalt))
	return err == nil
}

// GenerateToken returns a random unique token.
func GenerateToken() string {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		log.Error("unable to generate access token")
		return ""
	}

	return "ss_" + hex.EncodeToString(buf)
}

// HashToken hashes the token using sha256.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token + saltySalt))
	return hex.EncodeToString(sum[:])
}
