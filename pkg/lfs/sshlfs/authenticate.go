package sshlfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// Authenticate answers git-lfs-authenticate for user and repo on out: the
// HTTP LFS endpoint of repo under cfg.HTTP.PublicURL, and a token for it
// signed by tokens, the server's signer, valid for five minutes. operation
// is lfs.OperationDownload or lfs.OperationUpload, which the caller has
// authorized. It logs to logger, or to log.Default() when it is nil.
func Authenticate(out io.Writer, cfg *config.Config, tokens *jwk.Signer, logger *log.Logger, repo proto.Repository, user proto.User, operation string) error {
	if logger == nil {
		logger = log.Default()
	}
	logger = logger.WithPrefix("ssh.lfs-authenticate")
	if operation != lfs.OperationDownload && operation != lfs.OperationUpload {
		logger.Errorf("invalid operation: %s", operation)
		return errors.New("invalid operation")
	}
	if cfg == nil {
		return config.ErrNilConfig
	}

	if user == nil {
		// The SSH command refuses a caller without an account before it
		// gets here.
		logger.Debug("missing user")
		return proto.ErrUserNotFound
	}

	if repo == nil {
		logger.Errorf("missing repository")
		return proto.ErrRepoNotFound
	}

	expiresIn := time.Minute * 5
	expiresAt := time.Now().Add(expiresIn)
	j, err := tokens.Sign(user, []string{repo.Name()}, expiresAt)
	if err != nil {
		logger.Error("failed to sign token", "err", err)
		return err
	}

	href := fmt.Sprintf("%s/%s.git/info/lfs", cfg.HTTP.PublicURL, repo.Name())
	// The signed token is a bearer credential, so it is not logged.
	logger.Debug("generated token", "href", href, "expires_at", expiresAt)

	return json.NewEncoder(out).Encode(lfs.AuthenticateResponse{
		Header: map[string]string{
			"Authorization": fmt.Sprintf("Bearer %s", j),
		},
		Href:      href,
		ExpiresAt: expiresAt,
		ExpiresIn: int64(expiresIn / time.Second),
	})
}
