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
	tcexec "github.com/testcontainers/testcontainers-go/exec"
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

	container := startKeystoreFixture(ctx, t)

	probe := buildResidenceProbe(t)
	if err := container.CopyFileToContainer(ctx, probe, keystoreProbePath, 0o755); err != nil {
		t.Fatalf("copy residence probe: %v", err)
	}

	t.Run("keystore-absent", func(t *testing.T) {
		runResidenceMatrix(ctx, t, container, false)
	})

	t.Run("keystore-present", func(t *testing.T) {
		runResidenceMatrix(ctx, t, container, true)
	})
}

// TestKeystoreLinuxArtifactCarriesNoSecretServiceClient pins the mechanism
// behind the identity above from this repo's own side: the adapter compiled for
// Linux links no Secret Service client, so no keystore item can be created or
// read whatever a live service on the box offers.
func TestKeystoreLinuxArtifactCarriesNoSecretServiceClient(t *testing.T) {
	requireRunKeystore(t)

	binary := filepath.Join(t.TempDir(), "acp-go-opencode-linux")

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../cmd/acp-go-opencode")
	build.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

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

// startKeystoreFixture builds and starts the Secret Service fixture and
// registers its teardown.
func startKeystoreFixture(ctx context.Context, t *testing.T) testcontainers.Container {
	t.Helper()

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
		if terminateErr := container.Terminate(context.WithoutCancel(ctx)); terminateErr != nil {
			t.Errorf("terminate keystore fixture: %v", terminateErr)
		}
	})

	return container
}

// buildResidenceProbe compiles the package that owns the credential read path
// for the fixture's platform. Its claims cannot be made on the host: only the
// container carries a Secret Service to make the two Linux configurations
// differ.
func buildResidenceProbe(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "residence.test")

	command := exec.CommandContext(t.Context(), "go", "test", "-c", "-tags=integration", "-o", out, "./internal/opencode")
	command.Dir = ".."
	// GOWORK=off is load-bearing: a go.work in scope otherwise builds the probe
	// from another module's requirements.
	command.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build the residence probe: %v: %s", err, output)
	}

	return out
}

// runResidenceMatrix runs the probe in one configuration. The two runs differ
// only in whether the fixture's session bus is exported, which is what turns the
// matrix's identity claim into a comparison.
func runResidenceMatrix(ctx context.Context, t *testing.T, container testcontainers.Container, bus bool) {
	t.Helper()

	prelude := "unset DBUS_SESSION_BUS_ADDRESS; "
	if bus {
		prelude = ". " + keystoreEnvFile + "; export DBUS_SESSION_BUS_ADDRESS; "
	}

	command := prelude +
		"export " + envRunIntegration + "=1 " + envRunKeystore + "=1; " +
		"exec " + keystoreProbePath + " -test.v -test.run '^TestKeystoreResidenceMatrix$'"

	// The stream is demultiplexed so a frame header can never land inside the
	// result line this test matches on.
	code, output, err := container.Exec(ctx, []string{"/bin/sh", "-c", command}, tcexec.Multiplexed())
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

	// A skipped run also exits 0, which is the silent success this tier exists
	// to prevent, so the per-test result line is what reports the proof ran.
	if !strings.Contains(string(logs), "--- PASS: TestKeystoreResidenceMatrix") {
		t.Fatalf("the residence matrix reported no passing run: %s", logs)
	}
}
