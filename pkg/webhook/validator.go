package webhook

import (
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// Error aliases for backward compatibility.
var (
	ErrInvalidScheme = ssrf.ErrInvalidScheme
	ErrPrivateIP     = ssrf.ErrPrivateIP
	ErrInvalidURL    = ssrf.ErrInvalidURL
)

// ValidateWebhookURLAllowing validates that a webhook URL is safe to use: it
// must be http or https and reach only public addresses, apart from the
// private and internal destinations allowed permits (webhook.allowed_hosts).
// The same check runs again on every address at dial time; see
// ssrf.NewSecureClientAllowing.
func ValidateWebhookURLAllowing(rawURL string, allowed ssrf.AllowedHosts) error {
	return ssrf.ValidateURLAllowing(rawURL, allowed) //nolint:wrapcheck
}
