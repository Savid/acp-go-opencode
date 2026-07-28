//go:build integration

package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	envRunIntegration = "ACP_GO_OPENCODE_RUN_INTEGRATION"
	envRunKeystore    = "ACP_GO_OPENCODE_RUN_KEYSTORE"

	// envSessionBus reaches the Secret Service. Whether the fixture exported one
	// is the whole difference between the two Linux configurations.
	envSessionBus = "DBUS_SESSION_BUS_ADDRESS"

	// keystoreFixtureMarker is written by the credential-residence fixture's
	// entrypoint. Seeding a live Secret Service is only safe inside that
	// container, so the Linux configurations run nowhere else.
	keystoreFixtureMarker = "/run/acp-go-opencode-keystore/marker"

	// keystoreDarwinService is a service name this test owns end to end, so the
	// macOS third never reads, overwrites, or deletes a real login item.
	keystoreDarwinService = "acp-go-opencode-residence-canary"

	// keystoreLinuxService is the service name the harness documents for the
	// Secret Service, so the canary lands exactly where a keystore branch would
	// have looked.
	keystoreLinuxService = "opencode"
)

const (
	keystoreProvider    = "anthropic"
	keystoreFileCanary  = "canary-file-store-key"
	keystoreStoreCanary = "canary-keystore-key"
)

// TestKeystoreResidenceMatrix proves the identity OpenCode's residence answer
// rests on: the durable runtime store is the sole authority, and a keystore
// holding an item under this provider's own name changes neither which value the
// read path returns nor what it returns once that store is gone. The same body
// runs in all three configurations — Linux without a Secret Service, Linux with
// one, and macOS against its login keychain — so they are asserted to agree
// rather than assumed to.
func TestKeystoreResidenceMatrix(t *testing.T) {
	requireResidenceTier(t)
	seedResidenceKeystore(t)

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

	if !residenceKeystorePresent() {
		return
	}

	// The seeded item is still there and still readable, which is what makes the
	// absence above an answer about the read path rather than about the keystore.
	if seeded := lookupResidenceKeystore(t); seeded != keystoreStoreCanary {
		t.Fatalf("the seeded keystore item read back %q, want %q", seeded, keystoreStoreCanary)
	}
}

// requireResidenceTier answers to both tier gates. On Linux it additionally
// requires the fixture container: planting a canary in a developer's live Secret
// Service is not something a test may do, and the container is where the driver
// runs this binary once per Linux configuration.
func requireResidenceTier(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunKeystore) != "1" {
		t.Skipf("set %s=1 and %s=1 to run the credential-residence matrix",
			envRunIntegration, envRunKeystore)
	}

	if runtime.GOOS == "darwin" {
		return
	}

	if _, err := os.Stat(keystoreFixtureMarker); err != nil {
		t.Skipf("the Linux configurations run inside the keystore fixture container: %v", err)
	}
}

// residenceKeystorePresent reports whether this configuration carries a keystore
// at all. Linux without a session bus is the keystore-absent third, and it is
// the one configuration with nothing to seed and nothing to read back.
func residenceKeystorePresent() bool {
	return runtime.GOOS == "darwin" || os.Getenv(envSessionBus) != ""
}

// seedResidenceKeystore plants canary material through the platform's own tool
// rather than through the read path, so the assertion is not a round trip of one
// library against itself.
func seedResidenceKeystore(t *testing.T) {
	t.Helper()

	switch {
	case runtime.GOOS == "darwin":
		seedDarwinResidenceKeystore(t)
	case os.Getenv(envSessionBus) == "":
		t.Log("no session bus is exported: this is the keystore-absent configuration, which seeds nothing")
	default:
		seedSecretServiceResidenceKeystore(t)
	}
}

// seedDarwinResidenceKeystore writes the canary to the login keychain under a
// service name this test owns, and takes it back out again. A scratch HOME is
// not an alternative: a keychain write under one blocks forever on an
// interactive modal.
func seedDarwinResidenceKeystore(t *testing.T) {
	t.Helper()

	add := exec.Command("security", "add-generic-password", "-U",
		"-s", keystoreDarwinService, "-a", keystoreProvider, "-w", keystoreStoreCanary)

	if output, err := add.CombinedOutput(); err != nil {
		t.Fatalf("seed the login keychain canary: %v: %s", err, output)
	}

	t.Cleanup(func() {
		remove := exec.Command("security", "delete-generic-password",
			"-s", keystoreDarwinService, "-a", keystoreProvider)

		if output, err := remove.CombinedOutput(); err != nil {
			t.Errorf("delete the login keychain canary: %v: %s", err, output)
		}
	})
}

// seedSecretServiceResidenceKeystore stores the canary under the harness's own
// documented service and account names, which is where a keystore branch would
// have gone looking.
func seedSecretServiceResidenceKeystore(t *testing.T) {
	t.Helper()

	command := exec.Command("secret-tool", "store", "--label=opencode-canary",
		"service", keystoreLinuxService, "account", keystoreProvider)
	command.Stdin = strings.NewReader(keystoreStoreCanary)

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("seed the Secret Service canary: %v: %s", err, output)
	}
}

// lookupResidenceKeystore reads the canary back through the same platform tool
// that planted it.
func lookupResidenceKeystore(t *testing.T) string {
	t.Helper()

	var command *exec.Cmd

	if runtime.GOOS == "darwin" {
		command = exec.Command("security", "find-generic-password",
			"-s", keystoreDarwinService, "-a", keystoreProvider, "-w")
	} else {
		command = exec.Command("secret-tool", "lookup",
			"service", keystoreLinuxService, "account", keystoreProvider)
	}

	output, err := command.Output()
	if err != nil {
		t.Fatalf("look up the keystore canary: %v", err)
	}

	// security prints the password with a trailing newline; secret-tool does not.
	return strings.TrimRight(string(output), "\n")
}
