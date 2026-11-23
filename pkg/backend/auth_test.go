package backend

import (
	"testing"
)

func TestGenerateToken(t *testing.T) {
	t.Parallel()
	token := GenerateToken()
	if token == "" {
		t.Fatal("token is empty")
	}
}

func TestHashToken(t *testing.T) {
	t.Parallel()
	token := GenerateToken()
	hash := HashToken(token)
	if hash == "" {
		t.Fatal("hash is empty")
	}
}
