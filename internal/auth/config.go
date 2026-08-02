package auth

import (
	"fmt"
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
//
// There is no default. Running unguarded is allowed, but only when someone said
// so: an app that is running is one whose exposure was chosen rather than
// overlooked. Saying nothing, or saying half of it, is an error rather than a
// quiet fallback to either answer.
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
		// Contradictory. Letting the off switch win would mean a variable left
		// over from an afternoon's debugging quietly unguarding an app that
		// looks configured, so this asks which was meant.
		return Decision{}, fmt.Errorf(
			"%s is set to true but a provider is configured too; unset one of them to say which you meant",
			EnvDisabled)
	case disabled:
		return Decision{Disabled: true}, nil
	case configured:
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

func is(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
