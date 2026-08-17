//go:build !windows

package opencode

import (
	"net/url"
	"path/filepath"
)

func sessionCarrierFileURL(path string) string {
	return (&url.URL{Scheme: sessionCarrierURLScheme, Path: filepath.ToSlash(path)}).String()
}
