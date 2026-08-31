//go:build windows

package opencode

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

const windowsEnvironmentChildMarker = "acp-go-opencode-report-windows-environment"

type windowsChildEnvironment struct {
	Path     string `json:"path"`
	PathExt  string `json:"pathExt"`
	Provider string `json:"provider"`
}

func TestOrdinaryWindowsExecutableAndEnvironmentBehavior(t *testing.T) {
	if slices.Contains(os.Args, windowsEnvironmentChildMarker) {
		_ = json.NewEncoder(os.Stdout).Encode(windowsChildEnvironment{
			Path: os.Getenv("PATH"), PathExt: os.Getenv("PATHEXT"), Provider: os.Getenv("PROVIDER_KEY"),
		})
		os.Exit(0)
	}

	sourcePath, err := os.Executable()
	require.NoError(t, err)
	source, err := os.Open(sourcePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })

	directory := t.TempDir()
	targetPath := filepath.Join(directory, "opencode.exe")
	target, err := os.Create(targetPath)
	require.NoError(t, err)
	_, err = io.Copy(target, source)
	require.NoError(t, err)
	require.NoError(t, target.Close())

	hostile := t.TempDir()
	environment, err := buildProcessEnvironmentFrom(nil,
		map[string]string{"Path": hostile, "PathExt": ".NOPE", "Provider_Key": "base"},
		map[string]string{"PATH": directory, "PATHEXT": ".EXE;.CMD", "PROVIDER_KEY": "session"},
	)
	require.NoError(t, err)
	entries := envMapToSlice(environment)
	resolved, err := resolveOrdinaryProcessExecutable("opencode", entries)
	require.NoError(t, err)
	require.Equal(t, targetPath, resolved)

	command := exec.Command(resolved,
		"-test.run=^TestOrdinaryWindowsExecutableAndEnvironmentBehavior$",
		"--", windowsEnvironmentChildMarker,
	)
	command.Env = entries
	var output bytes.Buffer
	command.Stdout = &output
	require.NoError(t, command.Run())

	var child windowsChildEnvironment
	require.NoError(t, json.Unmarshal(output.Bytes(), &child))
	require.Equal(t, windowsChildEnvironment{
		Path: directory, PathExt: ".EXE;.CMD", Provider: "session",
	}, child)
}

// TestWindowsEnvironmentCollapsesRepeatedSpellingsDeterministically pins the
// two distinct rules that make two spellings of one Windows variable behave as
// one variable.
//
// Across ordered inputs — an entry slice, or two phases of composeEnvironment —
// the later value is the one the child reads, which is what lets an overlay
// replace an ambient value spelled differently. Within one unordered phase
// there is no "later": composeEnvironment sorts the keys and writes them in
// that order, so the lexicographically last spelling is the one that survives,
// and "Path" therefore beats "PATH". That value is arbitrary; that it is the
// same value on every run is not.
func TestWindowsEnvironmentCollapsesRepeatedSpellingsDeterministically(t *testing.T) {
	require.Equal(t, "selected", environmentValue([]string{"Path=discarded", "PATH=selected"}, pathEnv))
	require.Equal(t, "selected",
		composeEnvironment(map[string]string{"Path": "discarded"}, map[string]string{"PATH": "selected"})[pathEnv])

	within := map[string]string{"PATH": "sorts-first", "Path": "sorts-last"}
	require.Equal(t, []string{"PATH=sorts-last"}, envMapToSlice(composeEnvironment(within)),
		"one semantic key reaches the child, carrying the value of the spelling that sorts last")
	require.Equal(t, []string{"PATH=sorts-last"}, envMapToSlice(composeEnvironment(within)),
		"and the same spelling wins on every composition of the same phase")
}
