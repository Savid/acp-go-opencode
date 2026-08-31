package opencode

import (
	"os"
	"path/filepath"
	"strings"
)

// browserShimPrefix names every shim directory created beneath a scratch
// parent.
const browserShimPrefix = "acp-go-opencode-browser-shim-"

const browserShimBrowserEnv = "BROWSER"

// browserShimScript is what each shadowed launcher becomes: a program that
// accepts any arguments, opens nothing, and reports success. Launchers walk a
// candidate list and stop at the first success, so failing here would hand the
// URL to a candidate this directory does not shadow.
var browserShimScript = []byte("#!/bin/sh\nexit 0\n")

// browserLauncherNames are the programs a harness execs to open a URL. Darwin's
// `open` is the entry a BROWSER-only remedy misses: the launcher execs it
// directly and never reads BROWSER.
var browserLauncherNames = []string{
	"open",
	"xdg-open",
	"x-www-browser",
	"www-browser",
	"sensible-browser",
}

// BrowserShim is a scratch directory of no-op launchers that shadows every
// browser a native login leg would otherwise open on the operator's desktop.
// The caller that starts the leg owns the directory, because the leg outlives
// no single call inside this package.
type BrowserShim struct {
	dir string
}

// Dir names the directory the shim occupies.
func (s *BrowserShim) Dir() string {
	return s.dir
}

// environ returns env with the shim ahead of PATH and BROWSER pointed at one of
// its no-ops.
func (s *BrowserShim) environ(env []string) []string {
	return browserShimEnviron(env, s.dir)
}

// Remove deletes the shim directory.
func (s *BrowserShim) Remove() error {
	if s == nil {
		return nil
	}

	return os.RemoveAll(s.dir)
}

// browserShimEnviron rewrites a child environment so dir precedes every other
// PATH entry and BROWSER names a no-op inside it. Both mechanisms are set
// because harnesses split across them: one execs a launcher off PATH, another
// reads BROWSER, and neither alone covers both.
func browserShimEnviron(env []string, dir string) []string {
	kept := make([]string, 0, len(env)+2)
	search := dir

	if base := environmentValue(env, pathEnv); base != "" {
		search += string(os.PathListSeparator) + base
	}

	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			kept = append(kept, entry)

			continue
		}

		switch {
		case EnvironmentKeyEqual(key, pathEnv):
		case EnvironmentKeyEqual(key, browserShimBrowserEnv):
		default:
			kept = append(kept, entry)
		}
	}

	return append(kept,
		canonicalEnvironmentKey(pathEnv)+"="+search,
		canonicalEnvironmentKey(browserShimBrowserEnv)+"="+browserShimCommand(dir),
	)
}

// browserShimCommand is the value BROWSER carries: an absolute no-op that
// ignores the URL it is handed.
func browserShimCommand(dir string) string {
	return filepath.Join(dir, browserLauncherNames[0])
}
