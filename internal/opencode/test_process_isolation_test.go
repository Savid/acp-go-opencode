package opencode

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func testProcessIsolation() *ProcessIsolation {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid = 1
	}
	if gid == 0 {
		gid = 1
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			environment[key] = value
		}
	}
	if environment["PATH"] == "" {
		environment["PATH"] = "/usr/bin:/bin"
	}

	return &ProcessIsolation{UID: uint32(uid), GID: uint32(gid), BaseEnvironment: environment}
}

func withTestProcessIsolation(options StartOptions) StartOptions {
	if options.ProcessIsolation == nil {
		options.ProcessIsolation = testProcessIsolation()
	}

	return options
}

func skipUnprivilegedDarwinIsolation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("requires a privileged two-principal fixture to clear supplementary groups")
	}
}
