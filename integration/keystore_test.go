//go:build integration

package integration

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	keystoreEnvFile   = "/run/acp-go-opencode-keystore/env"
	keystoreRoundTrip = "/usr/local/bin/roundtrip.sh"
	keystoreProbePath = "/usr/local/bin/residence.test"
)

func requireKeystoreRuntime(t *testing.T) {
	t.Helper()
	requireRunKeystore(t)

	// The tier fails rather than skips once its gate is set: a silently green
	// residence suite is worse than a red one.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1 requires a container runtime: %v", envRunKeystore, err)
	}
}

// TestKeystoreLinuxCredentialResidence runs the two Linux thirds of the
// credential-residence matrix against a live Secret Service. OpenCode carries no
// keystore branch, so the claim under test is an identity: the durable runtime
// store answers the same way whether or not a secret service is on the box. Only
// running the read path beside a real service establishes it.
func TestKeystoreLinuxCredentialResidence(t *testing.T) {
	requireKeystoreRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    filepath.Join(".", "keystore"),
				Dockerfile: "Dockerfile",
				KeepImage:  true,
			},
			// Readiness is a store/lookup round trip executed in the container.
			// A log line and a bus-name check both report ready against a
			// service that answers no lookup.
			WaitingFor: wait.ForExec([]string{keystoreRoundTrip}).WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start keystore fixture: %v", err)
	}

	t.Cleanup(func() {
		if err := container.Terminate(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("terminate keystore fixture: %v", err)
		}
	})

	probe := buildLinuxProbe(t, "residence.test")

	if err := container.CopyFileToContainer(ctx, probe, keystoreProbePath, 0o755); err != nil {
		t.Fatalf("copy residence probe: %v", err)
	}

	matrix := keystoreProbePath + " -test.v -test.run '^TestKeystoreResidenceMatrix$'"

	for name, command := range map[string]string{
		"keystore-present": ". " + keystoreEnvFile + "; export DBUS_SESSION_BUS_ADDRESS; exec " + matrix,
		"keystore-absent":  "unset DBUS_SESSION_BUS_ADDRESS; exec " + matrix,
	} {
		t.Run(name, func(t *testing.T) {
			code, output, err := container.Exec(ctx, []string{"/bin/sh", "-c", command})
			if err != nil {
				t.Fatalf("run residence matrix: %v", err)
			}

			logs, readErr := io.ReadAll(output)
			if readErr != nil {
				t.Fatalf("read residence output: %v", readErr)
			}

			t.Log(string(logs))

			if code != 0 {
				t.Fatalf("residence matrix exited %d", code)
			}

			if strings.Contains(string(logs), "SKIP") {
				t.Fatalf("the residence matrix skipped inside the fixture: %s", logs)
			}
		})
	}
}

// TestKeystoreLinuxArtifactCarriesNoSecretServiceClient pins the mechanism
// behind the identity above from this repo's own side: the adapter compiled for
// Linux links no Secret Service client, so no keystore item can be created or
// read whatever a live service on the box offers.
func TestKeystoreLinuxArtifactCarriesNoSecretServiceClient(t *testing.T) {
	requireRunKeystore(t)

	binary := filepath.Join(t.TempDir(), "acp-go-opencode-linux")

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../cmd/acp-go-opencode")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the Linux artifact: %v: %s", err, output)
	}

	contents, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read the Linux artifact: %v", err)
	}

	for _, symbol := range []string{"libsecret", "org.freedesktop.secrets", "gnome-keyring"} {
		if strings.Contains(string(contents), symbol) {
			t.Fatalf("the Linux artifact carries %q", symbol)
		}
	}
}

// buildLinuxProbe compiles the package that owns the native runtime for the
// fixture's platform. Its claims cannot be made on the host: only the container
// carries the Secret Service and the launcher names Linux resolves.
func buildLinuxProbe(t *testing.T, name string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), name)

	command := exec.CommandContext(t.Context(), "go", "test", "-c", "-tags=integration", "-o", out, "./internal/opencode")
	command.Dir = ".."
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", name, err, output)
	}

	return out
}
