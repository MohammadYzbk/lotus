package secrets

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// stores returns each implementation, so both answer the same contract. The
// keychain runs against go-keyring's in-memory provider: a test must never
// write to the developer's real credential store.
func stores(t *testing.T) map[string]Store {
	t.Helper()
	keyring.MockInit()
	return map[string]Store{
		"keychain": &Keychain{Service: "lotus-test", Account: "github"},
		"memory":   &Memory{},
	}
}

func TestRoundTrip(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Get(); !errors.Is(err, ErrNotFound) {
				t.Errorf("empty store: got %v, want ErrNotFound", err)
			}
			if err := store.Set("gho_secret"); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get()
			if err != nil {
				t.Fatal(err)
			}
			if got != "gho_secret" {
				t.Errorf("got %q", got)
			}
		})
	}
}

// Disconnect has to succeed whatever the starting state, or a half-connected
// account becomes impossible to clear.
func TestClearIsIdempotent(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if err := store.Clear(); err != nil {
				t.Errorf("clearing an empty store failed: %v", err)
			}
			if err := store.Set("gho_secret"); err != nil {
				t.Fatal(err)
			}
			if err := store.Clear(); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get(); !errors.Is(err, ErrNotFound) {
				t.Errorf("after clear: got %v, want ErrNotFound", err)
			}
			if err := store.Clear(); err != nil {
				t.Errorf("second clear failed: %v", err)
			}
		})
	}
}

// Storing an empty token would leave the app "connected" to nothing, and every
// later request would fail with a confusing 401.
func TestSetRejectsEmpty(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if err := store.Set(""); err == nil {
				t.Error("an empty secret was stored")
			}
		})
	}
}
