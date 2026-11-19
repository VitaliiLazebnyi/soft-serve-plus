package backend

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// saltySalt is appended to an access token before it is hashed.
const saltySalt = "salty-soft-serve"

// GenerateToken returns a random unique token.
func GenerateToken() string {
	buf := make([]byte, 20)
	_, _ = rand.Read(buf) // crypto/rand.Read never fails.

	return "ss_" + hex.EncodeToString(buf)
}

// HashToken hashes the token using sha256.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token + saltySalt))
	return hex.EncodeToString(sum[:])
}
