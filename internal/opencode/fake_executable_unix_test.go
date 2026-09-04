//go:build !windows

package opencode

import "fmt"

// testExecutableName returns name unchanged. A POSIX executable is named by its
// mode, not by its extension.
func testExecutableName(name string) string { return name }

// fakeOpenCodeLauncherName is what the launcher is called on disk.
const fakeOpenCodeLauncherName = "fake-opencode"

// fakeOpenCodeLauncher is the launcher body that re-enters the test binary as
// the fake OpenCode server. The shebang is what makes it runnable here.
func fakeOpenCodeLauncher(executable string) string {
	return fmt.Sprintf(
		"#!/bin/sh\nACP_GO_OPENCODE_FAKE_SERVER_HELPER=1 exec %q -test.run=TestFakeOpenCodeServerProcessHelper -- \"$@\"\n",
		executable,
	)
}
