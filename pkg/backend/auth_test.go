package backend

import (
	"testing"
)

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
