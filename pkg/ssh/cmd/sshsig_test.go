package cmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// sshsigSign returns signer's armored SSHSIG signature over message in
// namespace, as ssh-keygen -Y sign makes it, with hash sha256 or sha512 and,
// for an RSA key, the signature algorithm algo ("" for the signer's
// default).
func sshsigSign(t *testing.T, signer gossh.Signer, namespace, hashAlg, algo string, message []byte) []byte {
	t.Helper()
	var digest []byte
	switch hashAlg {
	case "sha256":
		d := sha256.Sum256(message)
		digest = d[:]
	case "sha512":
		d := sha512.Sum512(message)
		digest = d[:]
	default:
		t.Fatalf("hash %q", hashAlg)
	}
	signed := append([]byte(sshsigMagic), gossh.Marshal(sshsigSignedData{
		Namespace:     namespace,
		HashAlgorithm: hashAlg,
		Hash:          digest,
	})...)
	var sig *gossh.Signature
	var err error
	if algo != "" {
		sig, err = signer.(gossh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, signed, algo)
	} else {
		sig, err = signer.Sign(rand.Reader, signed)
	}
	if err != nil {
		t.Fatal(err)
	}
	blob := append([]byte(sshsigMagic), gossh.Marshal(sshsigBlob{
		Version:       1,
		PublicKey:     signer.PublicKey().Marshal(),
		Namespace:     namespace,
		HashAlgorithm: hashAlg,
		Signature:     gossh.Marshal(sig),
	})...)
	b64 := base64.StdEncoding.EncodeToString(blob)
	var out strings.Builder
	out.WriteString(sshsigBegin + "\n")
	for len(b64) > 70 {
		out.WriteString(b64[:70] + "\n")
		b64 = b64[70:]
	}
	out.WriteString(b64 + "\n" + sshsigEnd + "\n")
	return []byte(out.String())
}

func newEd25519Signer(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifySSHSig(t *testing.T) {
	ed := newEd25519Signer(t)
	other := newEd25519Signer(t)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := gossh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	const ns = pubkeyProofNamespace
	msg := []byte("alice")

	good := sshsigSign(t, ed, ns, "sha512", "", msg)
	// The signature is at the end of the blob: change a character of the
	// last full line of base64.
	lines := strings.Split(string(good), "\n")
	last := lines[len(lines)-4]
	flip := byte('A')
	if last[10] == 'A' {
		flip = 'B'
	}
	lines[len(lines)-4] = last[:10] + string(flip) + last[11:]
	tampered := []byte(strings.Join(lines, "\n"))
	for _, tc := range []struct {
		name string
		pk   gossh.PublicKey
		ns   string
		msg  []byte
		sig  []byte
		ok   bool
	}{
		{"ed25519 sha512", ed.PublicKey(), ns, msg, good, true},
		{"ed25519 sha256", ed.PublicKey(), ns, msg, sshsigSign(t, ed, ns, "sha256", "", msg), true},
		{"rsa-sha2-512", rsaSigner.PublicKey(), ns, msg, sshsigSign(t, rsaSigner, ns, "sha512", gossh.KeyAlgoRSASHA512, msg), true},
		{"rsa-sha2-256", rsaSigner.PublicKey(), ns, msg, sshsigSign(t, rsaSigner, ns, "sha512", gossh.KeyAlgoRSASHA256, msg), true},
		{"rsa sha1", rsaSigner.PublicKey(), ns, msg, sshsigSign(t, rsaSigner, ns, "sha512", gossh.KeyAlgoRSA, msg), false},
		{"other message", ed.PublicKey(), ns, []byte("bob"), good, false},
		{"other namespace", ed.PublicKey(), "file", msg, good, false},
		{"other key", other.PublicKey(), ns, msg, good, false},
		{"signed by other key", ed.PublicKey(), ns, msg, sshsigSign(t, other, ns, "sha512", "", msg), false},
		{"tampered", ed.PublicKey(), ns, msg, tampered, false},
		{"empty", ed.PublicKey(), ns, msg, nil, false},
		{"not armored", ed.PublicKey(), ns, msg, []byte("U1NIU0lH"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifySSHSig(tc.pk, tc.ns, tc.msg, tc.sig)
			if tc.ok && err != nil {
				t.Fatalf("verify: %v", err)
			}
			if !tc.ok && !errors.Is(err, errBadSSHSig) {
				t.Fatalf("verify: %v, want %v", err, errBadSSHSig)
			}
		})
	}
}

// TestVerifySSHSigFromSSHKeygen checks a signature made by OpenSSH's
// ssh-keygen -Y sign, as the README tells users to make it.
func TestVerifySSHSigFromSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "id")
	for _, typ := range []string{"ed25519", "rsa"} {
		t.Run(typ, func(t *testing.T) {
			_ = os.Remove(key)
			_ = os.Remove(key + ".pub")
			if out, err := exec.CommandContext(t.Context(), keygen, "-q", "-t", typ, "-N", "", "-f", key).CombinedOutput(); err != nil {
				t.Fatalf("ssh-keygen: %v: %s", err, out)
			}
			c := exec.CommandContext(t.Context(), keygen, "-Y", "sign", "-n", pubkeyProofNamespace, "-f", key)
			c.Stdin = strings.NewReader("alice")
			sig, err := c.Output()
			if err != nil {
				t.Fatalf("ssh-keygen -Y sign: %v", err)
			}
			pubText, err := os.ReadFile(key + ".pub")
			if err != nil {
				t.Fatal(err)
			}
			pk, _, _, _, err := gossh.ParseAuthorizedKey(pubText)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifySSHSig(pk, pubkeyProofNamespace, []byte("alice"), sig); err != nil {
				t.Fatalf("verify: %v", err)
			}
			if err := verifySSHSig(pk, pubkeyProofNamespace, []byte("bob"), sig); !errors.Is(err, errBadSSHSig) {
				t.Fatalf("verify for another name: %v", err)
			}
		})
	}
}
