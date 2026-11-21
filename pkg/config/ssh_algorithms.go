package config

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
)

// DefaultSSHKexAlgorithms returns the key exchange algorithms the SSH server
// offers: every algorithm x/crypto considers secure, in its order of
// preference. This leaves out the SHA-1 key exchanges
// (diffie-hellman-group14-sha1 and older) that x/crypto still offers by
// default.
func DefaultSSHKexAlgorithms() []string {
	return slices.Clone(ssh.SupportedAlgorithms().KeyExchanges)
}

// DefaultSSHCiphers returns the ciphers the SSH server offers: every cipher
// x/crypto considers secure, in its order of preference.
func DefaultSSHCiphers() []string {
	return slices.Clone(ssh.SupportedAlgorithms().Ciphers)
}

// DefaultSSHMACs returns the MACs the SSH server offers: every MAC x/crypto
// considers secure except the SHA-1 based hmac-sha1, which x/crypto still
// lists as supported. hmac-sha1-96 is in x/crypto's insecure list and is
// left out as well.
func DefaultSSHMACs() []string {
	return slices.DeleteFunc(slices.Clone(ssh.SupportedAlgorithms().MACs), func(m string) bool {
		return m == ssh.HMACSHA1
	})
}

// DefaultSSHPublicKeyAuthAlgorithms returns the signature algorithms the
// SSH server accepts for client public key authentication when
// ssh.public_key_auth_algorithms is empty: every algorithm x/crypto
// considers secure. This leaves out the SHA-1 based ssh-rsa and ssh-dss
// signatures that x/crypto still accepts by default. RSA keys keep working
// through rsa-sha2-256 and rsa-sha2-512.
func DefaultSSHPublicKeyAuthAlgorithms() []string {
	return slices.Clone(ssh.SupportedAlgorithms().PublicKeyAuths)
}

// SSHPublicKeyAuthAlgorithms returns the signature algorithms the SSH
// server accepts for client public key authentication:
// ssh.public_key_auth_algorithms, or DefaultSSHPublicKeyAuthAlgorithms when
// it is empty.
func (c *SSHConfig) SSHPublicKeyAuthAlgorithms() []string {
	if len(c.PublicKeyAuthAlgorithms) == 0 {
		return DefaultSSHPublicKeyAuthAlgorithms()
	}
	return slices.Clone(c.PublicKeyAuthAlgorithms)
}

// validateAlgorithms trims ssh.public_key_auth_algorithms, drops blank
// entries so an unset environment variable does not read as one empty name,
// and rejects names x/crypto does not implement. x/crypto silently ignores
// unknown names, so without this a typo would quietly shrink the list, or
// leave it empty and fail every authentication.
func (c *SSHConfig) validateAlgorithms() error {
	supported := ssh.SupportedAlgorithms()
	insecure := ssh.InsecureAlgorithms()
	known := slices.Concat(supported.PublicKeyAuths, insecure.PublicKeyAuths)
	names := make([]string, 0, len(c.PublicKeyAuthAlgorithms))
	for _, n := range c.PublicKeyAuthAlgorithms {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !slices.Contains(known, n) {
			return fmt.Errorf("ssh.public_key_auth_algorithms: unknown algorithm %q; valid values are %s",
				n, strings.Join(known, ", "))
		}
		names = append(names, n)
	}
	c.PublicKeyAuthAlgorithms = names
	return nil
}
