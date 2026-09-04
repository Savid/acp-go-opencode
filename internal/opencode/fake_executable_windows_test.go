//go:build windows

package opencode

// testExecutableName gives name the extension Windows requires. A file with no
// extension is resolvable by nothing here: the product honours PATHEXT and
// CreateProcess reads no shebang, so a fixture the product must resolve has to
// carry one of PATHEXT's extensions.
func testExecutableName(name string) string { return name + ".exe" }

// fakeOpenCodeLauncherName is what the launcher is called on disk. The .cmd
// extension is both what PATHEXT resolves and what tells CreateProcess to hand
// the file to the command interpreter.
const fakeOpenCodeLauncherName = "fake-opencode.cmd"

// fakeOpenCodeLauncher is the launcher body that re-enters the test binary as
// the fake OpenCode server. It is a command script rather than a shell script,
// because that is the re-entry Windows can run: CreateProcess hands a .cmd to
// the command interpreter, which is how the environment marker and the
// forwarded arguments reach the test binary.
func fakeOpenCodeLauncher(executable string) string {
	return "@echo off\r\n" +
		"set ACP_GO_OPENCODE_FAKE_SERVER_HELPER=1\r\n" +
		`"` + executable + `" -test.run=TestFakeOpenCodeServerProcessHelper -- %*` + "\r\n"
}
