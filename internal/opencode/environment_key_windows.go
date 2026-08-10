//go:build windows

package opencode

import "strings"

func canonicalEnvironmentKey(key string) string { return strings.ToUpper(key) }

func environmentKeyEqual(left string, right string) bool { return strings.EqualFold(left, right) }
