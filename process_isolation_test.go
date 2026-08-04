package opencodeacp

import "testing"

func TestWithProcessIsolationClonesBaseEnvironment(t *testing.T) {
	base := map[string]string{"PATH": "/policy/bin", "ONLY_POLICY": "present"}
	options := applyOptions([]Option{WithProcessIsolation(ProcessIsolation{UID: 12, GID: 34, BaseEnvironment: base})})
	base["ONLY_POLICY"] = "mutated"
	if options.ProcessIsolation == nil || options.ProcessIsolation.BaseEnvironment["ONLY_POLICY"] != "present" {
		t.Fatal("WithProcessIsolation did not clone the base environment")
	}
}
