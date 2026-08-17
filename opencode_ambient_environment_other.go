//go:build !windows

package opencodeacp

func canonicalAmbientEnvironmentKey(key string) string { return key }
