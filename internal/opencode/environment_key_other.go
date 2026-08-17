//go:build !windows

package opencode

func canonicalEnvironmentKey(key string) string { return key }

// EnvironmentKeyEqual reports whether two environment names address the same
// variable. Outside Windows they are distinct variables unless spelled
// identically, so PATH and Path name two different things.
func EnvironmentKeyEqual(left string, right string) bool { return left == right }
