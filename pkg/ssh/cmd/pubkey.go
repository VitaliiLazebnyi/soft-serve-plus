package cmd

import (
	"context"
	"errors"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// errListedAdminKey is returned when a non-admin tries to attach a key that
// is listed in initial_admin_keys.
var errListedAdminKey = errors.New("this key is listed in initial_admin_keys; only an admin can attach it to an account (user add-pubkey)")

// errRemoveLastKey is returned when a user tries to remove their only public
// key, which would leave the account with no way to sign in over SSH.
var errRemoveLastKey = errors.New("refusing to remove your last public key: add another key first (pubkey add), or ask an admin to remove the account")

// errRemoveSessionKey is returned when a user tries to remove the public key
// the current session authenticated with.
var errRemoveSessionKey = errors.New("refusing to remove the key this session authenticated with: connect with another of your keys to remove it")

// checkRemovablePubkey reports why the caller may not remove pk from their
// own account, or nil if they may.
//
// The caller is always identified by the session key (see userByPublicKey),
// so that key is one of theirs: removing it would sign them out of the
// account they are using, and when it is their only key, lock them out for
// good. Removing a different key of theirs is allowed, so a key can be
// rotated by connecting with the new one.
func checkRemovablePubkey(ctx context.Context, user proto.User, pk gossh.PublicKey) error {
	sessionKey := sshutils.PublicKeyFromContext(ctx)
	if sessionKey == nil || !sshutils.KeysEqual(pk, sessionKey) {
		return nil
	}
	if len(user.PublicKeys()) <= 1 {
		return errRemoveLastKey
	}
	return errRemoveSessionKey
}

// PubkeyCommand returns a command that manages user public keys.
func PubkeyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pubkey",
		Aliases: []string{"pubkeys", "publickey", "publickeys"},
		Short:   "Manage your public keys",
	}

	pubkeyAddCommand := &cobra.Command{
		Use:   "add AUTHORIZED_KEY",
		Short: "Add a public key",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			user, err := userByPublicKey(ctx)
			if err != nil {
				return err
			}

			apk, _, err := sshutils.ParseAuthorizedKey(strings.Join(args, " "))
			if err != nil {
				return err
			}

			// Adding a key proves nothing about holding its private half,
			// so a non-admin may not attach a key listed in
			// initial_admin_keys. (Sessions with a listed key owned by a
			// non-admin are refused anyway; see
			// backend.ErrAdminKeyNonAdminOwner.) The error says why, so a new admin
			// knows to ask an admin rather than retry. That tells the
			// caller the key is listed, which is harmless: it is a public
			// key, and listing grants nothing without the private half.
			if !isServerAdmin(ctx) && IsPublicKeyAdmin(ctx, apk) {
				return errListedAdminKey
			}

			return be.AddPublicKey(ctx, user.Username(), apk)
		},
	}

	pubkeyRemoveCommand := &cobra.Command{
		Use:   "remove AUTHORIZED_KEY",
		Args:  cobra.MinimumNArgs(1),
		Short: "Remove a public key",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			user, err := userByPublicKey(ctx)
			if err != nil {
				return err
			}

			apk, _, err := sshutils.ParseAuthorizedKey(strings.Join(args, " "))
			if err != nil {
				return err
			}

			if err := checkRemovablePubkey(ctx, user, apk); err != nil {
				return err
			}

			// The check above used the key list read when the session
			// looked the user up. RemoveOwnPublicKey counts again under a
			// lock, so a removal racing in from another of the user's
			// sessions cannot leave the account with no keys.
			err = be.RemoveOwnPublicKey(ctx, user.Username(), apk)
			if errors.Is(err, backend.ErrLastPublicKey) {
				return errRemoveLastKey
			}
			return err
		},
	}

	pubkeyListCommand := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List public keys",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			user, err := userByPublicKey(ctx)
			if err != nil {
				return err
			}

			pks := user.PublicKeys()
			for _, pk := range pks {
				cmd.Println(sshutils.MarshalAuthorizedKey(pk))
			}

			return nil
		},
	}

	cmd.AddCommand(
		pubkeyAddCommand,
		pubkeyRemoveCommand,
		pubkeyListCommand,
	)

	return cmd
}
