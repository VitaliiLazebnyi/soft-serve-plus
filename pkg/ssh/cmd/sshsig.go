package cmd

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	gossh "golang.org/x/crypto/ssh"
)

// pubkeyProofNamespace is the SSHSIG namespace of the signature that proves a
// non-admin holds the private half of a key they add with `pubkey add`. The
// signed message is the account's username, with or without one trailing
// newline. See the README for the ssh-keygen command.
const pubkeyProofNamespace = "pubkey-add@soft-serve-plus"

// errBadSSHSig is returned for a signature that does not verify, whatever
// the reason; the reason is in the wrapped text only.
var errBadSSHSig = errors.New("invalid signature")

// sshsigMagic starts an SSHSIG blob and the data it signs (PROTOCOL.sshsig).
const sshsigMagic = "SSHSIG"

// sshsigArmor are the lines an armored SSHSIG signature, as ssh-keygen -Y
// sign writes it, starts and ends with.
const (
	sshsigBegin = "-----BEGIN SSH SIGNATURE-----"
	sshsigEnd   = "-----END SSH SIGNATURE-----"
)

// sshsigBlob is an SSHSIG signature after the magic preamble.
type sshsigBlob struct {
	Version       uint32
	PublicKey     []byte
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Signature     []byte
}

// sshsigSignedData is what an SSHSIG signature signs, after the magic
// preamble.
type sshsigSignedData struct {
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Hash          []byte
}

// verifySSHSig checks that armored, an armored SSHSIG signature, is pk's
// signature over message in namespace, as ssh-keygen -Y verify does. The
// signature must carry pk itself, not a certificate for it, and an RSA
// signature must use SHA-2: SSHSIG does not allow ssh-rsa.
func verifySSHSig(pk gossh.PublicKey, namespace string, message, armored []byte) error {
	text := strings.TrimSpace(string(armored))
	body, ok := strings.CutPrefix(text, sshsigBegin)
	if !ok {
		return fmt.Errorf("%w: not an armored SSH signature", errBadSSHSig)
	}
	body, ok = strings.CutSuffix(body, sshsigEnd)
	if !ok {
		return fmt.Errorf("%w: not an armored SSH signature", errBadSSHSig)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return fmt.Errorf("%w: %w", errBadSSHSig, err)
	}
	rest, ok := bytes.CutPrefix(raw, []byte(sshsigMagic))
	if !ok {
		return fmt.Errorf("%w: not an SSHSIG signature", errBadSSHSig)
	}
	var blob sshsigBlob
	if err := gossh.Unmarshal(rest, &blob); err != nil {
		return fmt.Errorf("%w: %w", errBadSSHSig, err)
	}
	if blob.Version != 1 {
		return fmt.Errorf("%w: unsupported version %d", errBadSSHSig, blob.Version)
	}
	signer, err := gossh.ParsePublicKey(blob.PublicKey)
	if err != nil {
		return fmt.Errorf("%w: %w", errBadSSHSig, err)
	}
	if !sshutils.KeysEqual(signer, pk) {
		return fmt.Errorf("%w: signed with another key", errBadSSHSig)
	}
	if blob.Namespace != namespace {
		return fmt.Errorf("%w: namespace %q, want %q", errBadSSHSig, blob.Namespace, namespace)
	}
	var h hash.Hash
	switch blob.HashAlgorithm {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return fmt.Errorf("%w: unsupported hash %q", errBadSSHSig, blob.HashAlgorithm)
	}
	var sig gossh.Signature
	if err := gossh.Unmarshal(blob.Signature, &sig); err != nil {
		return fmt.Errorf("%w: %w", errBadSSHSig, err)
	}
	if sig.Format == gossh.KeyAlgoRSA {
		return fmt.Errorf("%w: RSA signatures must use SHA-2", errBadSSHSig)
	}
	h.Write(message)
	signed := append([]byte(sshsigMagic), gossh.Marshal(sshsigSignedData{
		Namespace:     blob.Namespace,
		Reserved:      blob.Reserved,
		HashAlgorithm: blob.HashAlgorithm,
		Hash:          h.Sum(nil),
	})...)
	if err := pk.Verify(signed, &sig); err != nil {
		return fmt.Errorf("%w: %w", errBadSSHSig, err)
	}
	return nil
}
