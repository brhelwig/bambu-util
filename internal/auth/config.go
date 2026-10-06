package auth

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Environment variables the app is configured with.
const (
	EnvIssuer       = "OIDC_ISSUER"
	EnvClientID     = "OIDC_CLIENT_ID"
	EnvClientSecret = "OIDC_CLIENT_SECRET"
	EnvPublicURL    = "PUBLIC_URL"
	EnvDisabled     = "AUTH_DISABLED"
)

// Decision is what the environment says to do about authentication.
type Decision struct {
	// Config is the provider to use. Zero when authentication is switched off.
	Config Config
	// Disabled is true when the app was expressly told to run without a login.
	Disabled bool
}

// Decide reads the environment and works out whether to require a login.
// There is no default: either a full provider config or AUTH_DISABLED=true is
// required, and anything else is an error.
func Decide(env func(string) string) (Decision, error) {
	cfg := Config{
		Issuer:       strings.TrimSpace(env(EnvIssuer)),
		ClientID:     strings.TrimSpace(env(EnvClientID)),
		ClientSecret: strings.TrimSpace(env(EnvClientSecret)),
		PublicURL:    strings.TrimSpace(env(EnvPublicURL)),
	}
	set := map[string]string{
		EnvIssuer:       cfg.Issuer,
		EnvClientID:     cfg.ClientID,
		EnvClientSecret: cfg.ClientSecret,
		EnvPublicURL:    cfg.PublicURL,
	}
	var missing []string
	for name, value := range set {
		if value == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	disabled := strings.TrimSpace(env(EnvDisabled)) == "true"
	configured := len(missing) == 0
	partly := len(missing) > 0 && len(missing) < len(set)

	switch {
	case disabled && (configured || partly):
		// Contradictory; don't let a leftover AUTH_DISABLED silently win.
		return Decision{}, fmt.Errorf(
			"%s is set to true but a provider is configured too; unset one of them to say which you meant",
			EnvDisabled)
	case disabled:
		return Decision{Disabled: true}, nil
	case configured:
		public, err := checkPublicURL(cfg.PublicURL)
		if err != nil {
			return Decision{}, err
		}
		cfg.PublicURL = public
		return Decision{Config: cfg}, nil
	case partly:
		return Decision{}, fmt.Errorf(
			"the login provider is half configured: %s %s missing. Set them, or set %s=true to run with no login at all",
			strings.Join(missing, ", "), is(len(missing)), EnvDisabled)
	default:
		return Decision{}, fmt.Errorf(
			"nothing was said about authentication. Set %s, %s, %s and %s to require a login, or %s=true to run with none",
			EnvIssuer, EnvClientID, EnvClientSecret, EnvPublicURL, EnvDisabled)
	}
}

// checkPublicURL checks PUBLIC_URL is a bare scheme and host, and returns it
// without a trailing slash.
func checkPublicURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s=%q is not a URL: %w", EnvPublicURL, raw, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%s=%q needs to start with https:// (or http://)", EnvPublicURL, raw)
	case u.Host == "":
		return "", fmt.Errorf("%s=%q has no host", EnvPublicURL, raw)
	case u.User != nil, u.RawQuery != "", u.Fragment != "", u.ForceQuery:
		return "", fmt.Errorf("%s=%q should be just the scheme and host, like https://printer.example.com", EnvPublicURL, raw)
	case u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("%s=%q has a path; the app has to be served from the root of its host", EnvPublicURL, raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

func is(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
