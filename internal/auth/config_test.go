package auth

import (
	"strings"
	"testing"
)

// env turns a map into the lookup Decide reads, so a test states an environment
// rather than setting one on the process.
func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

var complete = map[string]string{
	EnvIssuer:       "https://id.example.com",
	EnvClientID:     "printer",
	EnvClientSecret: "shhh",
	EnvPublicURL:    "https://printer.example.com",
}

func TestAFullyConfiguredProviderIsUsed(t *testing.T) {
	got, err := Decide(env(complete))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Disabled {
		t.Error("a configured provider was read as authentication being off")
	}
	if got.Config.Issuer != complete[EnvIssuer] || got.Config.ClientID != complete[EnvClientID] {
		t.Errorf("config = %+v, want it read from the environment", got.Config)
	}
}

func TestAuthenticationCanBeSwitchedOffOnPurpose(t *testing.T) {
	got, err := Decide(env(map[string]string{EnvDisabled: "true"}))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.Disabled {
		t.Error("the off switch was not honoured")
	}
}

// The point of the whole arrangement: an app that says nothing does not start,
// so nothing is ever left unguarded because somebody forgot.
func TestSayingNothingIsRefused(t *testing.T) {
	_, err := Decide(env(nil))
	if err == nil {
		t.Fatal("an app with nothing configured was allowed to start")
	}
	// The message has to be the fix, not just the complaint.
	for _, want := range []string{EnvIssuer, EnvClientID, EnvClientSecret, EnvPublicURL, EnvDisabled} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %s: %v", want, err)
		}
	}
}

// Half a configuration is the dangerous case: it looks configured.
func TestAHalfConfiguredProviderIsRefused(t *testing.T) {
	for _, missing := range []string{EnvIssuer, EnvClientID, EnvClientSecret, EnvPublicURL} {
		partial := map[string]string{}
		for name, value := range complete {
			if name != missing {
				partial[name] = value
			}
		}
		_, err := Decide(env(partial))
		if err == nil {
			t.Errorf("started with %s missing", missing)
			continue
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("the refusal does not name the missing %s: %v", missing, err)
		}
	}
}

// Both answers at once is contradictory. Letting the off switch win would mean
// a leftover variable quietly unguarding an app that looks configured.
func TestConfiguringBothIsRefused(t *testing.T) {
	both := map[string]string{EnvDisabled: "true"}
	for name, value := range complete {
		both[name] = value
	}
	_, err := Decide(env(both))
	if err == nil {
		t.Fatal("a contradictory configuration was accepted")
	}
	if !strings.Contains(err.Error(), EnvDisabled) {
		t.Errorf("the refusal does not mention %s: %v", EnvDisabled, err)
	}
}

// The off switch has to be deliberate, so only the exact word counts. Anything
// else is refused rather than quietly read as either answer.
func TestOnlyTheExactWordSwitchesAuthenticationOff(t *testing.T) {
	for _, value := range []string{"", "1", "yes", "TRUE", "true "} {
		got, err := Decide(env(map[string]string{EnvDisabled: value}))
		if value == "true " {
			// Surrounding space is trimmed: a trailing space in a compose file
			// is a typo, not a different intention.
			if err != nil || !got.Disabled {
				t.Errorf("%q should still switch it off: %v", value, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s=%q started the app without deciding anything", EnvDisabled, value)
		}
	}
}
