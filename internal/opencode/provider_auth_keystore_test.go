//go:build integration

package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// keystoreFixtureMarker is written by the credential-residence fixture's
// entrypoint. The matrix answers only inside that container, where a live
// Secret Service and its session bus exist.
const keystoreFixtureMarker = "/run/acp-go-opencode-keystore/marker"

const (
	keystoreProvider    = "anthropic"
	keystoreFileCanary  = "canary-file-store-key"
	keystoreStoreCanary = "canary-keystore-key"
)

// TestKeystoreResidenceMatrix proves the identity OpenCode's residence answer
// rests on: the durable runtime store is the sole authority, and a Secret
// Service holding an item under this provider's own name changes neither which
// value the read path returns nor what it returns once that store is gone. The
// same body runs with the session bus present and with it absent, so the two
// Linux configurations are asserted to agree rather than assumed to.
func TestKeystoreResidenceMatrix(t *testing.T) {
	if _, err := os.Stat(keystoreFixtureMarker); err != nil {
		t.Skip("the credential-residence matrix runs inside the keystore fixture container")
	}

	secretService := os.Getenv("DBUS_SESSION_BUS_ADDRESS") != ""
	if secretService {
		seedKeystoreCanary(t, keystoreStoreCanary)
	}

	data := t.TempDir()
	store := filepath.Join(data, authStoreDir, authStoreFile)

	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		t.Fatal(err)
	}

	body := `{"` + keystoreProvider + `":{"type":"api","key":"` + keystoreFileCanary + `"}}`
	if err := os.WriteFile(store, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	credential, present, err := readProviderAuthStore(data, keystoreProvider)
	if err != nil || !present {
		t.Fatalf("the durable runtime store answered nothing: %v/%v", present, err)
	}

	if credential.Key != keystoreFileCanary {
		t.Fatalf("the read path answered %q, want the runtime store's %q", credential.Key, keystoreFileCanary)
	}

	// Removing the store leaves the keystore item alone and the read fail-closed.
	// A read path that had a keystore branch would answer the seeded value here.
	if err := os.Remove(store); err != nil {
		t.Fatal(err)
	}

	credential, present, err = readProviderAuthStore(data, keystoreProvider)
	if err != nil || present {
		t.Fatalf("an absent runtime store did not answer absence: %v/%v/%v", credential.Key, present, err)
	}

	if !secretService {
		return
	}

	// The seeded item is still there and still readable, which is what makes
	// the absence above an answer about the read path rather than about the
	// service.
	if seeded := lookupKeystoreCanary(t); seeded != keystoreStoreCanary {
		t.Fatalf("the seeded keystore item read back %q, want %q", seeded, keystoreStoreCanary)
	}
}

// seedKeystoreCanary plants canary material through the platform tool rather
// than through the read path, so the assertion is not a round trip of one
// library against itself.
func seedKeystoreCanary(t *testing.T, contents string) {
	t.Helper()

	command := exec.Command("secret-tool", "store", "--label=opencode-canary",
		"service", "opencode", "account", keystoreProvider)
	command.Stdin = strings.NewReader(contents)

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("seed keystore canary: %v: %s", err, output)
	}
}

func lookupKeystoreCanary(t *testing.T) string {
	t.Helper()

	output, err := exec.Command("secret-tool", "lookup", "service", "opencode", "account", keystoreProvider).Output()
	if err != nil {
		t.Fatalf("look up keystore canary: %v", err)
	}

	return string(output)
}
