package ssh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

// The flow the README gives for `pubkey add`, over a real SSH session: the
// signature ssh-keygen -Y sign makes with the new key is piped into the
// command, which reads it from the session's input. In a session with a pty
// the command refuses at once rather than wait for a terminal's input.
func TestPubkeyAddProofOverSSH(t *testing.T) {
	t.Parallel()
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found")
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, ctx := startTestServer(t, cfg)
	be := depsOf(ctx).be

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{signer.PublicKey()}}); err != nil {
		t.Fatal(err)
	}

	key := filepath.Join(t.TempDir(), "id_new")
	if out, err := exec.CommandContext(t.Context(), keygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	sign := exec.CommandContext(t.Context(), keygen, "-Y", "sign", "-n", "pubkey-add@soft-serve-plus", "-f", key)
	sign.Stdin = strings.NewReader("alice")
	proof, err := sign.Output()
	if err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v", err)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "alice",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         sessionLoopsWait,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	run := func(pty bool, stdin []byte) (string, error) {
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close() //nolint: errcheck
		if pty {
			if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
				t.Fatal(err)
			}
		}
		var stderr bytes.Buffer
		s.Stdin = bytes.NewReader(stdin)
		s.Stderr = &stderr
		err = s.Run(`pubkey add "` + strings.TrimSpace(string(pub)) + `"`)
		return stderr.String(), err
	}

	// In a pty the input is a terminal, so the command refuses at once,
	// even with a valid proof on that input, and says what to do.
	if out, err := run(true, proof); err == nil || !strings.Contains(out, "needs proof") || !strings.Contains(out, "terminal") {
		t.Fatalf("pubkey add in a pty: %v: %q", err, out)
	}
	if out, err := run(false, proof); err != nil {
		t.Fatalf("pubkey add with the proof: %v: %q", err, out)
	}
	u, err := be.User(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(u.PublicKeys()); n != 2 {
		t.Fatalf("alice has %d keys, want 2", n)
	}
}
