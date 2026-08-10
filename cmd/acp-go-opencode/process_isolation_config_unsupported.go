//go:build !linux

package main

import "fmt"

// loadProcessIsolationConfig refuses off Linux. Only the explicitly supplied
// hardened policy is Linux-only; omitting the flag still runs ordinary
// same-identity native mode on this platform.
func loadProcessIsolationConfig(string) (processIsolationConfig, error) {
	return processIsolationConfig{}, fmt.Errorf("explicit process isolation is supported only on linux")
}
