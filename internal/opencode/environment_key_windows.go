//go:build windows

package opencode

import "strings"

func canonicalEnvironmentKey(key string) string { return strings.ToUpper(key) }

// EnvironmentKeyEqual reports whether two environment names address the same
// variable. Windows resolves environment names case-insensitively, so PATH and
// Path name one variable there.
func EnvironmentKeyEqual(left string, right string) bool { return strings.EqualFold(left, right) }
