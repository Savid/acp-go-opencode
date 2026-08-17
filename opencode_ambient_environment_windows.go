//go:build windows

package opencodeacp

import "strings"

func canonicalAmbientEnvironmentKey(key string) string { return strings.ToUpper(key) }
