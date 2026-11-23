package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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

// errPubkeyTaken is returned by `pubkey add` for a key already attached to an
// account. It does not say which account, nor whether it is the caller's.
var errPubkeyTaken = errors.New("this key is already attached to an account")

// errNoPubkeyProof is returned by `pubkey add` when a non-admin sends no
// valid proof of holding the key's private half; see noPubkeyProof.
var errNoPubkeyProof = errors.New("pubkey add needs proof that you hold the key's private half")

// noPubkeyProof returns errNoPubkeyProof, with why the proof was refused
// and the command that makes one for name, the caller's username.
func (cmds *Commands) noPubkeyProof(ctx context.Context, name string, why error) error {
	return fmt.Errorf("%w (%w); pipe in an SSH signature of your username made with that key, "+
		"for example: printf %%s %s | ssh-keygen -Y sign -n %s -f PRIVATE_KEY | %s pubkey add \"$(cat PRIVATE_KEY.pub)\"",
		errNoPubkeyProof, why, name, pubkeyProofNamespace, SSHCommand(cmds.cfg))
}

// maxPubkeyProofSize bounds the signature `pubkey add` reads from its input.
// An armored SSH signature made with the largest RSA key is under 3 KiB.
const maxPubkeyProofSize = 16 << 10

// errProofInTerminal is why a proof is refused in a session with a pty: the
// input is the user's terminal, and reading it would wait for a proof that
// is not coming.
var errProofInTerminal = errors.New("the input is a terminal")

// errProofTooLong is why a proof longer than maxPubkeyProofSize is refused.
var errProofTooLong = errors.New("the input is too long for one")

// checkPubkeyProof reads an armored SSHSIG signature (ssh-keygen -Y sign)
// from the command's input and checks that it is pk's signature, in
// pubkeyProofNamespace, over the account's username as it is now. The
// message may end with one newline, as echo writes it.
func (cmds *Commands) checkPubkeyProof(cmd *cobra.Command, user proto.User, pk gossh.PublicKey) error {
	ctx := cmd.Context()
	// The name is read again by ID: the account may have been renamed
	// since the session started.
	cur, err := cmds.be.UserByID(ctx, user.ID())
	if err != nil {
		if errors.Is(err, proto.ErrUserNotFound) {
			return errUnknownKey(ctx)
		}
		return lookupFailed(ctx, cmds.logger, "look up the session's user", err)
	}
	name := cur.Username()

	if s := SessionFromContext(ctx); s != nil {
		if _, _, isPty := s.Pty(); isPty {
			return cmds.noPubkeyProof(ctx, name, errProofInTerminal)
		}
	}
	proof, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxPubkeyProofSize+1))
	if err != nil {
		return err
	}
	if len(proof) > maxPubkeyProofSize {
		return cmds.noPubkeyProof(ctx, name, errProofTooLong)
	}
	err = verifySSHSig(pk, pubkeyProofNamespace, []byte(name), proof)
	if err != nil && verifySSHSig(pk, pubkeyProofNamespace, []byte(name+"\n"), proof) == nil {
		err = nil
	}
	if err != nil {
		return cmds.noPubkeyProof(ctx, name, err)
	}
	return nil
}

// checkRemovablePubkey reports why the caller may not remove pk from their
// own account, or nil if they may.
//
// The caller is always identified by the session key (see userByPublicKey),
// so that key is one of theirs: removing it would sign them out of the
// account they are using, and when it is their only key, lock them out for
// good. Removing a different key of theirs is allowed, so a key can be
// rotated by connecting with the new one.
func checkRemovablePubkey(ctx context.Context, user proto.User, pk gossh.PublicKey) error {
	sessionKey := proto.PublicKeyFromContext(ctx)
	if sessionKey == nil || !proto.KeysEqual(pk, sessionKey) {
		return nil
	}
	if len(user.PublicKeys()) <= 1 {
		return errRemoveLastKey
	}
	return errRemoveSessionKey
}

// PubkeyCommand returns a command that manages user public keys.
func (cmds *Commands) PubkeyCommand() *cobra.Command {
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
			be := cmds.be
			user, err := cmds.userByPublicKey(ctx)
			if err != nil {
				return err
			}

			apk, _, err := proto.ParseAuthorizedKey(strings.Join(args, " "))
			if err != nil {
				return err
			}

			// A non-admin may not attach a key listed in
			// initial_admin_keys. (Sessions with a listed key owned by a
			// non-admin are refused anyway; see
			// backend.AdminKeyNonAdminOwnerError.) The error says why, so a new admin
			// knows to ask an admin rather than retry. That tells the
			// caller the key is listed, which is harmless: it is a public
			// key, and listing grants nothing without the private half.
			admin := cmds.isServerAdmin(ctx)
			if !admin && be.IsAdminKey(apk) {
				return errListedAdminKey
			}
			// A non-admin proves they hold the key's private half, so
			// nobody can claim a key that is not theirs, such as a
			// published one, and later act for its holder. Admins attach
			// keys for others anyway (user add-pubkey).
			if !admin {
				if err := cmds.checkPubkeyProof(cmd, user, apk); err != nil {
					return err
				}
			}

			err = be.AddOwnPublicKey(ctx, user, proto.PublicKeyFromContext(ctx), apk)
			if errors.Is(err, db.ErrDuplicateKey) {
				// Whose key it is, the caller's or another account's, is
				// not said.
				return errPubkeyTaken
			}
			return err
		},
	}

	pubkeyRemoveCommand := &cobra.Command{
		Use:   "remove AUTHORIZED_KEY",
		Args:  cobra.MinimumNArgs(1),
		Short: "Remove a public key",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := cmds.be
			user, err := cmds.userByPublicKey(ctx)
			if err != nil {
				return err
			}

			apk, _, err := proto.ParseAuthorizedKey(strings.Join(args, " "))
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
			err = be.RemoveOwnPublicKey(ctx, user, apk)
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
			user, err := cmds.userByPublicKey(ctx)
			if err != nil {
				return err
			}

			pks := user.PublicKeys()
			for _, pk := range pks {
				cmd.Println(proto.MarshalAuthorizedKey(pk))
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
