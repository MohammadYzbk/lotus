// Package secrets keeps the GitHub token in the OS credential store.
//
// The token is the one piece of state that must never reach settings.json, a
// log line, or a project directory. Keeping it behind an interface means the
// rest of the app only ever handles it as a value it just fetched, and tests
// never touch the real keychain.
package secrets

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// ErrNotFound means nothing is stored — the user has not connected an account.
var ErrNotFound = errors.New("secrets: nothing stored")

// Store holds one secret.
type Store interface {
	// Get returns the stored secret, or ErrNotFound.
	Get() (string, error)
	Set(secret string) error
	// Clear removes the secret. Removing what is not there is not an error:
	// "disconnect" should succeed whatever the starting state.
	Clear() error
}

// Keychain is the OS credential store: Keychain on macOS, Credential Manager
// on Windows, and the Secret Service on Linux.
type Keychain struct {
	Service string
	Account string
}

// NewKeychain returns the store the app uses for a GitHub token.
func NewKeychain() *Keychain {
	return &Keychain{Service: "lotus", Account: "github"}
}

func (k *Keychain) Get() (string, error) {
	secret, err := keyring.Get(k.Service, k.Account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("secrets: read: %w", err)
	}
	return secret, nil
}

func (k *Keychain) Set(secret string) error {
	if secret == "" {
		return errors.New("secrets: refusing to store an empty secret")
	}
	if err := keyring.Set(k.Service, k.Account, secret); err != nil {
		return fmt.Errorf("secrets: write: %w", err)
	}
	return nil
}

func (k *Keychain) Clear() error {
	err := keyring.Delete(k.Service, k.Account)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("secrets: delete: %w", err)
}

// Memory is a Store for tests, and for a run where the credential store is
// unavailable: better to hold the token for the session than to refuse to
// sign in at all.
type Memory struct{ secret string }

func (m *Memory) Get() (string, error) {
	if m.secret == "" {
		return "", ErrNotFound
	}
	return m.secret, nil
}

func (m *Memory) Set(secret string) error {
	if secret == "" {
		return errors.New("secrets: refusing to store an empty secret")
	}
	m.secret = secret
	return nil
}

func (m *Memory) Clear() error {
	m.secret = ""
	return nil
}
