package domain

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// trackingParams are marketing parameters that make otherwise identical product
// URLs look different. Stripping them keeps the per-user uniqueness check sane.
var trackingParams = []string{
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
	"gclid", "yclid", "fbclid", "_openstat", "ref", "referrer", "from",
}

// NormalizeURL validates a user-supplied product URL and returns its canonical
// form. It rejects anything that is not a public http(s) address, which also
// blocks the obvious SSRF shapes (localhost, link-local, private ranges).
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w: empty URL", ErrValidation)
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%w: cannot parse URL: %w", ErrValidation, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("%w: unsupported scheme %q", ErrValidation, u.Scheme)
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: URL has no host", ErrValidation)
	}
	if err := checkPublicHost(host); err != nil {
		return "", err
	}

	u.Host = host
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	u.Fragment = ""
	u.User = nil

	q := u.Query()
	for _, p := range trackingParams {
		q.Del(p)
	}
	u.RawQuery = q.Encode()

	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

func checkPublicHost(host string) error {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("%w: local addresses are not allowed", ErrValidation)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("%w: non-public IP address is not allowed", ErrValidation)
		}
	}
	return nil
}

// HostOf returns the hostname of a normalised URL, or an empty string.
// It is used for per-host rate limiting in the scraper.
func HostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
