//go:build !windows

package opencode

func canonicalEnvironmentKey(key string) string { return key }

func environmentKeyEqual(left string, right string) bool { return left == right }
