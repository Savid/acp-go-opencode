//go:build windows

package opencode

// sessionCarrierShellWrapperSource is empty on Windows. OpenCode starts cmd
// with /c and PowerShell with -NoProfile, and neither reruns a startup file
// that could rewrite the search path, so the environment the hook returns is
// already the environment the command runs under.
//
// With no wrapper to stand in for a login shell there is nothing to address:
// the plugin publishes no marker and no login-shell address on this platform,
// only the operation's own directories ahead of the runtime's search path.
func sessionCarrierShellWrapperSource(string) string { return "" }
