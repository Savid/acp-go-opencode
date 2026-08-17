package opencode

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDarwinGenerationCommandMarkersAndFinish(t *testing.T) {
	if err := (*DarwinGeneration)(nil).prepareCommand(nil); err == nil {
		t.Fatal("nil generation accepted a command")
	}

	generation := &DarwinGeneration{RuntimeID: "runtime", ScratchRoot: "/scratch"}
	cmd := exec.Command("tool")
	cmd.Env = []string{
		"KEEP=value",
		DarwinRuntimeIDEnv + "=stale",
		DarwinScratchRootEnv + "=stale",
	}
	if err := generation.prepareCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(cmd.Env) != 3 || cmd.Env[0] != "KEEP=value" || cmd.Env[1] != DarwinRuntimeIDEnv+"=runtime" || cmd.Env[2] != DarwinScratchRootEnv+"=/scratch" {
		t.Fatalf("prepared environment = %#v", cmd.Env)
	}

	if err := (*DarwinGeneration)(nil).started(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := generation.started(1, 1); err != nil {
		t.Fatal(err)
	}
	started := false
	generation.RecordStarted = func(pid, pgid int) error {
		started = pid == 2 && pgid == 2

		return errors.New("record start")
	}
	if err := generation.started(2, 2); err == nil || !started {
		t.Fatalf("started result = %v, called=%v", err, started)
	}

	if err := (*DarwinGeneration)(nil).finish(true); err != nil {
		t.Fatal(err)
	}
	finishCalls := 0
	finishGeneration := &DarwinGeneration{RecordFinished: func(bool) error {
		finishCalls++

		return errors.New("finish failed")
	}}
	if err := finishGeneration.finish(false); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("finish error = %v", err)
	}
	if err := finishGeneration.finish(true); !errors.Is(err, ErrProcessContainmentIncomplete) || finishCalls != 1 {
		t.Fatalf("memoized finish = %v, calls=%d", err, finishCalls)
	}

	parent := t.TempDir()
	if !pathWithin(parent, filepath.Join(parent, "child")) || pathWithin(parent, parent) || pathWithin(parent, filepath.Dir(parent)) {
		t.Fatal("path containment mismatch")
	}
}
