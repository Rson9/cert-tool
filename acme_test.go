package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The happy path must work with zero configuration: an empty e-mail is
// allowed (registered without a contact address).
func TestLoadACMEUserWithoutEmail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "account")

	user, err := loadACMEUser(dir, "")
	if err != nil {
		t.Fatalf("loadACMEUser with empty e-mail should succeed: %v", err)
	}
	if user.GetPrivateKey() == nil {
		t.Fatal("account key was not created")
	}
	keyPath := filepath.Join(dir, accountKeyFile)
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("account key file missing: %v", err)
	}

	// Second call reuses the same key.
	before, _ := os.ReadFile(keyPath)
	if _, err := loadACMEUser(dir, "admin@example.com"); err != nil {
		t.Fatalf("loadACMEUser second call failed: %v", err)
	}
	after, _ := os.ReadFile(keyPath)
	if string(before) != string(after) {
		t.Error("account key must not be regenerated on later runs")
	}
}
