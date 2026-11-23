package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/matryer/is"
	"go.yaml.in/yaml/v4"
	"golang.org/x/crypto/ssh"
)

func TestSSHAlgorithmDefaultsExcludeSHA1(t *testing.T) {
	sha1 := []string{
		ssh.InsecureKeyExchangeDH14SHA1, ssh.InsecureKeyExchangeDH1SHA1, ssh.InsecureKeyExchangeDHGEXSHA1,
		ssh.HMACSHA1, ssh.InsecureHMACSHA196,
	}
	for _, list := range [][]string{DefaultSSHKexAlgorithms(), DefaultSSHCiphers(), DefaultSSHMACs()} {
		if len(list) == 0 {
			t.Error("empty default algorithm list")
		}
		for _, a := range list {
			if slices.Contains(sha1, a) {
				t.Errorf("default algorithms include %q", a)
			}
		}
	}
	if !slices.Equal(DefaultSSHCiphers(), ssh.SupportedAlgorithms().Ciphers) {
		t.Errorf("default ciphers = %v, want x/crypto's supported ciphers", DefaultSSHCiphers())
	}
}

func TestSSHPublicKeyAuthDefaultsExcludeSHA1(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.SSH.PublicKeyAuthAlgorithms) != 0 {
		t.Fatalf("public key auth algorithms set by default: %v", cfg.SSH.PublicKeyAuthAlgorithms)
	}
	got := cfg.SSH.AcceptedPublicKeyAuthAlgorithms()
	for _, a := range []string{ssh.KeyAlgoRSA, ssh.InsecureKeyAlgoDSA} { //nolint:staticcheck // checks the deprecated algorithm stays off
		if slices.Contains(got, a) {
			t.Errorf("default public key auth algorithms include %q", a)
		}
	}
	for _, a := range []string{ssh.KeyAlgoED25519, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoECDSA256} {
		if !slices.Contains(got, a) {
			t.Errorf("default public key auth algorithms %v lack %q", got, a)
		}
	}
}

func TestParseSSHAlgorithms(t *testing.T) {
	is := is.New(t)
	t.Setenv("SOFT_SERVE_SSH_PUBLIC_KEY_AUTH_ALGORITHMS", "")
	is.NoErr(os.Unsetenv("SOFT_SERVE_SSH_PUBLIC_KEY_AUTH_ALGORITHMS"))

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	is.NoErr(os.WriteFile(path, []byte(`ssh:
  public_key_auth_algorithms: ["ssh-ed25519", "ssh-rsa"]
`), 0o600))
	cfg := DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(parseFile(cfg, path))
	is.Equal(cfg.SSH.AcceptedPublicKeyAuthAlgorithms(), []string{"ssh-ed25519", "ssh-rsa"})

	t.Setenv("SOFT_SERVE_SSH_PUBLIC_KEY_AUTH_ALGORITHMS", "rsa-sha2-512, ssh-dss")
	cfg = DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(parseEnv(cfg))
	is.Equal(cfg.SSH.AcceptedPublicKeyAuthAlgorithms(), []string{"rsa-sha2-512", "ssh-dss"})

	// Hooks re-read the config from the environment.
	is.True(slices.Contains(cfg.Environ(), "SOFT_SERVE_SSH_PUBLIC_KEY_AUTH_ALGORITHMS=rsa-sha2-512,ssh-dss"))

	// An empty variable, as Environ writes for an unset list, keeps the
	// default rather than reading as one empty name.
	t.Setenv("SOFT_SERVE_SSH_PUBLIC_KEY_AUTH_ALGORITHMS", "")
	cfg = DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(parseEnv(cfg))
	is.Equal(len(cfg.SSH.PublicKeyAuthAlgorithms), 0)
	is.Equal(cfg.SSH.AcceptedPublicKeyAuthAlgorithms(), DefaultSSHPublicKeyAuthAlgorithms())
}

func TestValidateRejectsUnknownSSHAlgorithms(t *testing.T) {
	for _, algos := range [][]string{
		{"rsa-sha2-384"},
		// A name valid in another list is not valid here.
		{"aes128-ctr"},
		// Certificate algorithms are checked by their underlying key.
		{ssh.CertAlgoED25519v01},
	} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.SSH.PublicKeyAuthAlgorithms = algos
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%v: Validate accepted an unknown algorithm", algos)
			continue
		}
		if !strings.Contains(err.Error(), "ssh.public_key_auth_algorithms") || !strings.Contains(err.Error(), "unknown algorithm") {
			t.Errorf("%v: unclear error %q", algos, err)
		}
	}

	// Legacy algorithms x/crypto still implements are accepted, for
	// operators who must re-enable them.
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.PublicKeyAuthAlgorithms = ssh.InsecureAlgorithms().PublicKeyAuths
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected legacy algorithms: %v", err)
	}
}

// TestSSHAlgorithmDocsMatchDefaults keeps the default lists and the legacy
// examples in the README and the generated config file in step with the
// defaults the server uses.
func TestSSHAlgorithmDocsMatchDefaults(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Git may check README.md out with CRLF line endings (the windows CI
	// runner does), so compare lines without the carriage returns.
	doc := strings.ReplaceAll(string(readme), "\r\n", "\n")

	// The bullet list of defaults.
	for _, l := range []struct {
		prefix string
		want   []string
	}{
		{"- key exchange: ", DefaultSSHKexAlgorithms()},
		{"- ciphers: ", DefaultSSHCiphers()},
		{"- MACs: ", DefaultSSHMACs()},
		{"- public key authentication: ", withoutMLDSA(DefaultSSHPublicKeyAuthAlgorithms())},
	} {
		i := strings.Index(doc, l.prefix)
		if i < 0 {
			t.Fatalf("README has no %q line", l.prefix)
		}
		line := doc[i+len(l.prefix):]
		line = line[:strings.IndexByte(line, '\n')]
		line = strings.ReplaceAll(line, " (and its alias `curve25519-sha256@libssh.org`)", "")
		var got []string
		for _, n := range strings.Split(line, ", ") {
			got = append(got, strings.Trim(n, "`"))
		}
		if !slices.Equal(got, l.want) {
			t.Errorf("README %q lists %v, want %v", l.prefix, got, l.want)
		}
	}

	wantPubKeyAuths := append(withoutMLDSA(DefaultSSHPublicKeyAuthAlgorithms()), ssh.KeyAlgoRSA)
	checkPubKeyAuths := func(name, example string) {
		t.Helper()
		var cfg Config
		if err := yaml.Unmarshal([]byte(example), &cfg); err != nil {
			t.Fatalf("%s legacy example: %v", name, err)
		}
		if !slices.Equal(cfg.SSH.PublicKeyAuthAlgorithms, wantPubKeyAuths) {
			t.Errorf("%s legacy example public_key_auth_algorithms = %v, want %v",
				name, cfg.SSH.PublicKeyAuthAlgorithms, wantPubKeyAuths)
		}
	}

	// The README legacy example is the yaml block after "Older clients".
	rest := doc[strings.Index(doc, "**Older clients.**"):]
	start := strings.Index(rest, "```yaml\n") + len("```yaml\n")
	readmeExample := rest[start : start+strings.Index(rest[start:], "```")]
	checkPubKeyAuths("README", readmeExample)

	// And the note about RSA keys further down.
	checkRSANote(t, doc)

	// The config file has the public key authentication example as comment
	// lines after "only signs with ssh-rsa".
	file := string(defaultConfigFile)
	var b strings.Builder
	b.WriteString("ssh:\n")
	lines := strings.Split(file[strings.Index(file, "only signs with ssh-rsa"):], "\n")
	for len(lines) > 0 && !strings.HasPrefix(lines[0], "  #   ") {
		lines = lines[1:]
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "  #   ") {
			break
		}
		b.WriteString("  " + strings.TrimPrefix(l, "  #   ") + "\n")
	}
	checkPubKeyAuths("config file", b.String())
}

// checkRSANote checks that the README's note about RSA keys, in doc, says what
// the defaults do: SHA-2 RSA signatures are accepted and SHA-1 ssh-rsa ones
// refused unless configured, not that ssh-rsa is accepted.
func checkRSANote(t *testing.T, doc string) {
	t.Helper()
	i := strings.Index(doc, "## A note about RSA keys")
	if i < 0 {
		t.Fatal("README has no note about RSA keys")
	}
	note := doc[i+len("## A note about RSA keys"):]
	note = note[:strings.Index(note, "\n## ")]
	// One sentence per line, to look at what each says.
	sentences := strings.Split(strings.ReplaceAll(strings.ReplaceAll(note, "\n", " "), ". ", ".\n"), "\n")

	defaults := DefaultSSHPublicKeyAuthAlgorithms()
	for _, algo := range []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512} {
		if !slices.Contains(defaults, algo) {
			t.Fatalf("%s is not accepted by default", algo)
		}
		if !strings.Contains(note, "`"+algo+"`") {
			t.Errorf("the RSA note does not name %s, which is accepted by default", algo)
		}
	}
	if slices.Contains(defaults, ssh.KeyAlgoRSA) {
		t.Fatal("ssh-rsa is accepted by default")
	}
	// Every sentence that names ssh-rsa must say, in these words, that it
	// is refused unless added: a sentence that negates the refusal, or
	// says it is accepted, does not match.
	refused := regexp.MustCompile("^It refuses the SHA-1 `ssh-rsa` signatures of older clients unless `ssh-rsa` is appended to `ssh\\.public_key_auth_algorithms`")
	var found bool
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if !strings.Contains(s, "`"+ssh.KeyAlgoRSA+"`") {
			continue
		}
		found = true
		if !refused.MatchString(s) {
			t.Errorf("the RSA note says of ssh-rsa %q; want %q", s, refused)
		}
	}
	if !found {
		t.Error("the RSA note does not say what happens to ssh-rsa signatures")
	}
}

// withoutMLDSA drops the ML-DSA algorithms, which x/crypto supports only
// when built with Go 1.27 or later. The docs list them separately.
func withoutMLDSA(algos []string) []string {
	return slices.DeleteFunc(algos, func(a string) bool {
		return a == ssh.KeyAlgoMLDSA44 || a == ssh.KeyAlgoMLDSA65 || a == ssh.KeyAlgoMLDSA87
	})
}
