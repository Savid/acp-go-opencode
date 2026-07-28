//go:build integration

package integration

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

const browserShimProbePath = "/usr/local/bin/browser-shim.test"

// TestKeystoreLinuxLoginNeverExecsABrowserLauncher runs the launcher-containment
// proof where Linux resolves it: against a real PATH holding xdg-open,
// x-www-browser, www-browser, and sensible-browser. The development host offers
// only Darwin's open, so nothing there exercises the names an unneutralised
// Linux login leg would reach to complete a grant in the operator's browser.
func TestKeystoreLinuxLoginNeverExecsABrowserLauncher(t *testing.T) {
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
			Entrypoint: []string{"/bin/sleep", "infinity"},
			// The probe's temporary directories land on this mount. The runtime
			// home takes a local lock the image's overlay root cannot offer, and
			// exec is explicit because Docker mounts tmpfs noexec while the
			// launchers under test have to run.
			Tmpfs:      map[string]string{"/tmp": "rw,exec,size=256m"},
			WaitingFor: wait.ForExec([]string{"/bin/true"}),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start launcher fixture: %v", err)
	}

	t.Cleanup(func() {
		if terminateErr := container.Terminate(context.WithoutCancel(ctx)); terminateErr != nil {
			t.Errorf("terminate launcher fixture: %v", terminateErr)
		}
	})

	probe := buildLinuxProbe(t, "browser-shim.test")

	if copyErr := container.CopyFileToContainer(ctx, probe, browserShimProbePath, 0o755); copyErr != nil {
		t.Fatalf("copy launcher probe: %v", copyErr)
	}

	// The stream is demultiplexed so a frame header can never land inside the
	// result line this test matches on.
	code, output, err := container.Exec(ctx, []string{
		browserShimProbePath, "-test.run", "^TestLoginNeverExecsABrowserLauncher$", "-test.v",
	}, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("run launcher probe: %v", err)
	}

	logs, readErr := io.ReadAll(output)
	if readErr != nil {
		t.Fatalf("read launcher probe output: %v", readErr)
	}

	t.Log(string(logs))

	if code != 0 {
		t.Fatalf("launcher probe exited %d", code)
	}

	// A selector that matches nothing also exits 0 and prints PASS, so the
	// per-test result line is what reports the proof actually ran.
	if !strings.Contains(string(logs), "--- PASS: TestLoginNeverExecsABrowserLauncher") {
		t.Fatalf("the launcher probe reported no passing run: %s", logs)
	}
}
