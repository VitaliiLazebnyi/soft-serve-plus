package serve

import (
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testnet"
)

// TestMain makes HTTP clients without a transport of their own retry a
// loopback connect that timed out (see testnet.Dial).
func TestMain(m *testing.M) {
	testnet.UseDialForDefaultTransport()
	os.Exit(m.Run())
}
