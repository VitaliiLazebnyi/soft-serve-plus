package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// LFSAuthenticate implements the Git LFS SSH authentication command.
// Context must have *config.Config, *log.Logger, proto.User.
// cmd.Args should have the repo path and operation as arguments.
func LFSAuthenticate(ctx context.Context, cmd ServiceCommand) error {
	if len(cmd.Args) < 2 {
		return errors.New("missing args")
	}

	logger := log.FromContext(ctx).WithPrefix("ssh.lfs-authenticate")
	operation := cmd.Args[1]
	if operation != lfs.OperationDownload && operation != lfs.OperationUpload {
		logger.Errorf("invalid operation: %s", operation)
		return errors.New("invalid operation")
	}

	user := proto.UserFromContext(ctx)
	if user == nil {
		// The SSH command refuses a caller without an account before it
		// gets here.
		logger.Debug("missing user")
		return proto.ErrUserNotFound
	}

	repo := proto.RepositoryFromContext(ctx)
	if repo == nil {
		logger.Errorf("missing repository")
		return proto.ErrRepoNotFound
	}

	cfg := config.FromContext(ctx)
	expiresIn := time.Minute * 5
	expiresAt := time.Now().Add(expiresIn)
	j, err := signToken(cfg, user, []string{repo.Name()}, expiresAt)
	if err != nil {
		logger.Error("failed to sign token", "err", err)
		return err
	}

	href := fmt.Sprintf("%s/%s.git/info/lfs", cfg.HTTP.PublicURL, repo.Name())
	// The signed token is a bearer credential, so it is not logged.
	logger.Debug("generated token", "href", href, "expires_at", expiresAt)

	return json.NewEncoder(cmd.Stdout).Encode(lfs.AuthenticateResponse{
		Header: map[string]string{
			"Authorization": fmt.Sprintf("Bearer %s", j),
		},
		Href:      href,
		ExpiresAt: expiresAt,
		ExpiresIn: int64(expiresIn / time.Second),
	})
}

// signToken signs a token for user with the host key cfg loaded; see
// jwk.Signer.Sign.
func signToken(cfg *config.Config, user proto.User, audience []string, expiresAt time.Time) (string, error) {
	kp, err := config.KeyPair(cfg)
	if err != nil {
		return "", fmt.Errorf("%w: %w", jwk.ErrHostKey, err)
	}
	signer, err := jwk.NewSigner(kp, cfg.HTTP.PublicURL)
	if err != nil {
		return "", err
	}
	return signer.Sign(user, audience, expiresAt)
}
