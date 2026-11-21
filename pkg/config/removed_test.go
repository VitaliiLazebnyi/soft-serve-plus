package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A config.yaml an earlier version wrote holds the removed settings with
// the values it wrote then. They give no warning: every upgraded server
// would warn at each start about values its operator never set, and the
// server does what they say. Another value is still reported.
func TestRemovedSettingsAtOldDefaultsAreSilent(t *testing.T) {
	old := `ssh:
  handshake_timeout: 120
  max_sessions: 10
  kex_algorithms: []
  ciphers: []
  macs: []
db:
  max_open_conns: 0
  max_idle_conns: 0
  conn_max_lifetime: 0
  conn_max_idle_time: 0
jobs:
  mirror_pull: "@every 10m"
  lfs_sweep: "@every 1h"
max_blob_size: 1048576
`
	loadEnv(t, old)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RemovedSettings(); len(got) != 0 {
		t.Errorf("RemovedSettings() = %+v, want none for the values earlier versions wrote", got)
	}
	if got := cfg.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %+v, want none", got)
	}

	for _, changed := range []string{
		"  max_sessions: 3\n",
		"  kex_algorithms: [\"curve25519-sha256\"]\n",
		"  handshake_timeout: \"120\"\n", // quoted, still the same text
	} {
		file := strings.Replace(old, "  max_sessions: 10\n", "", 1)
		file = strings.Replace(file, "  kex_algorithms: []\n", "", 1)
		file = strings.Replace(file, "  handshake_timeout: 120\n", "", 1)
		file = strings.Replace(file, "ssh:\n", "ssh:\n"+changed, 1)
		loadEnv(t, file)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		got := cfg.RemovedSettings()
		want := 1
		if strings.Contains(changed, `"120"`) {
			want = 0
		}
		if len(got) != want {
			t.Errorf("%q: RemovedSettings() = %+v, want %d", strings.TrimSpace(changed), got, want)
		}
		for _, r := range got {
			if !r.InFile {
				t.Errorf("%s: InFile = false for a config file key", r.Key)
			}
		}
	}
}

// The README's Removed settings table lists every removed setting with its
// variable, and states the fixed values the server uses instead.
func TestReadmeRemovedSettingsTable(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(readme), "\r\n", "\n")
	i := strings.Index(doc, "#### Removed settings")
	if i < 0 {
		t.Fatal("README has no Removed settings section")
	}
	doc = doc[i:]
	doc = doc[:strings.Index(doc[1:], "\n#")+1]

	rows := map[string][]string{}
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) != 3 {
			t.Fatalf("table row %q has %d cells, want 3", line, len(cells))
		}
		rows[strings.Trim(cells[0], "`")] = cells
	}
	if len(rows) != len(removedSettings) {
		t.Errorf("the table has %d rows, want one per removed setting (%d)", len(rows), len(removedSettings))
	}
	for _, r := range removedSettings {
		row, ok := rows[r.key]
		if !ok {
			t.Errorf("the table has no row for %s", r.key)
			continue
		}
		if row[1] != "`"+r.env+"`" {
			t.Errorf("%s: variable %s, want %s", r.key, row[1], r.env)
		}
	}

	if SSHHandshakeTimeout%time.Minute != 0 || MaxBlobSize%(1<<20) != 0 {
		t.Fatal("the table states the handshake timeout in minutes and the blob size in MiB")
	}
	for key, want := range map[string]string{
		"ssh.max_sessions":      fmt.Sprintf("allows %d sessions", SSHMaxSessions),
		"ssh.handshake_timeout": fmt.Sprintf("gives a client %d minutes", SSHHandshakeTimeout/time.Minute),
		"max_blob_size":         fmt.Sprintf("up to %d MiB", MaxBlobSize>>20),
	} {
		if row := rows[key]; row == nil || !strings.Contains(row[2], want) {
			t.Errorf("%s: the table says %q, want %q", key, row, want)
		}
	}
}
