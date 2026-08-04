package opencodeacp

import (
	"errors"
	"runtime"
	"strings"
)

const (
	platformDarwin  = "darwin"
	platformLinux   = "linux"
	platformWindows = "windows"

	privateAdapterEnvPrefix = "ACP_" + "GO_OPENCODE_INTERNAL_"
)

var runtimeGOOS = runtime.GOOS

func containmentMode(options Options) RuntimeContainmentMode {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return RuntimeContainmentUnavailable
	}

	switch runtimeGOOS {
	case platformLinux:
		return RuntimeContainmentAuthoritative
	case platformDarwin:
		if options.DarwinBestEffortContainment {
			return RuntimeContainmentBestEffort
		}
	}

	return RuntimeContainmentUnavailable
}

func validateContainmentOptions(options Options) error {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return errors.New("darwin best-effort containment is supported only on darwin")
	}

	for key := range options.Env {
		if strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) {
			return errors.New("environment uses the reserved " + privateAdapterEnvPrefix + " prefix")
		}
	}

	return nil
}
