package testscript

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/crypto/ssh"
)

// cmdWebhookDeliveries returns the webhook-deliveries command:
//
//	webhook-deliveries <repo> <webhook-id> <count> [regexp]
//
// It waits, for up to webhookWait (a minute), until the webhook has at least
// count recorded deliveries, successful or not, and the list matches regexp
// if one is given, and then writes the delivery list to stdout, as "soft
// repo webhook deliver list" does. The server sends push events, like every
// other event, in the background after the push has returned, so a script
// that reads the list right after a push must wait for the records first.
// The regexp is for a list that pruning keeps at a fixed length.
func cmdWebhookDeliveries(user string, keys ...ssh.Signer) func(ts *testscript.TestScript, neg bool, args []string) {
	return func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 3 || len(args) > 4 {
			ts.Fatalf("usage: webhook-deliveries <repo> <webhook-id> <count> [regexp]")
		}
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 0 {
			ts.Fatalf("webhook-deliveries: count must be a number of deliveries, not %q", args[2])
		}
		var re *regexp.Regexp
		if len(args) == 4 {
			re, err = regexp.Compile(args[3])
			ts.Check(err)
		}
		cmd := "repo webhook deliver list " + args[0] + " " + args[1]
		deadline := time.Now().Add(webhookWait)
		for {
			out, err := runSoftCommand(ts, user, keys, cmd)
			got := strings.Count(out, "✅") + strings.Count(out, "❌")
			if err == nil && got >= n && (re == nil || re.MatchString(out)) {
				_, _ = fmt.Fprint(ts.Stdout(), out)
				return
			}
			if time.Now().After(deadline) {
				ts.Fatalf("webhook-deliveries: %d of %d deliveries recorded within %v (last error %v):\n%s", got, n, webhookWait, err, out)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// runSoftCommand runs cmd over SSH as user and returns its stdout.
func runSoftCommand(ts *testscript.TestScript, user string, keys []ssh.Signer, cmd string) (string, error) {
	cli, err := ssh.Dial(
		"tcp",
		net.JoinHostPort(loopback, ts.Getenv("SSH_PORT")),
		&ssh.ClientConfig{
			User:            user,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(keys...)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		},
	)
	if err != nil {
		return "", err
	}
	defer cli.Close() //nolint:errcheck

	sess, err := cli.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close() //nolint:errcheck

	var out bytes.Buffer
	sess.Stdout = &out
	err = sess.Run(cmd)
	return out.String(), err
}
