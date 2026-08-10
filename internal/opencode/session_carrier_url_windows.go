//go:build windows

package opencode

import (
	"net/url"
	"path/filepath"
	"strings"
)

func sessionCarrierFileURL(path string) string {
	slash := filepath.ToSlash(path)
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash
	}

	return (&url.URL{Scheme: sessionCarrierURLScheme, Path: slash}).String()
}
