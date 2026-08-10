//go:build !linux

package opencode

func handoffSessionCarrierGeneratedTree(_ string, _ *ProcessIsolation) error { return nil }
