package opencodeacp

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	platformDarwin              = "darwin"
	platformLinux               = "linux"
	platformWindows             = "windows"
	managedEnvXDGDataHome       = "XDG_DATA_HOME"
	managedEnvOpenCodeConfigDir = "OPENCODE_CONFIG_DIR"

	privateAdapterEnvPrefix = "ACP_" + "GO_OPENCODE_INTERNAL_"
)

var runtimeGOOS = runtime.GOOS

// containmentMode reports the boundary a launch built from these options
// actually reaches. Omitting the policy is ordinary same-identity execution:
// it needs no privilege, works wherever the native launch itself works, and is
// therefore reported on every platform rather than only on the one that can
// harden. An explicit policy is the hardened Linux boundary and nothing else,
// so it is unavailable everywhere the hardening cannot be honored.
func containmentMode(options Options) RuntimeContainmentMode {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return RuntimeContainmentUnavailable
	}

	if options.ProcessIsolation != nil {
		if runtimeGOOS != platformLinux || options.DarwinBestEffortContainment {
			return RuntimeContainmentUnavailable
		}

		return RuntimeContainmentAuthoritative
	}

	if runtimeGOOS == platformDarwin && options.DarwinBestEffortContainment {
		return RuntimeContainmentBestEffort
	}

	return RuntimeContainmentSharedIdentity
}

func validateContainmentOptions(options Options) error {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return errors.New("darwin best-effort containment is supported only on darwin")
	}

	if options.ProcessIsolation != nil {
		if options.DarwinBestEffortContainment {
			return errors.New("explicit process isolation cannot be combined with darwin best-effort containment")
		}

		if runtimeGOOS != platformLinux {
			return errors.New("explicit process isolation is supported only on linux")
		}
	}

	for key := range options.Env {
		if reservedOpenCodeEnvKey(key) {
			return errors.New("environment key " + key + " is reserved for OpenCode runtime management")
		}
	}

	if options.ProcessIsolation != nil && options.Home != "" {
		if err := validateDurableHomePath(options.Home); err != nil {
			return err
		}
	}

	return nil
}

func validateDurableHomePath(path string) error {
	if path == "" || len(path) > 4096 || path == "/" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("OpenCode runtime home must be a canonical absolute non-root path of at most 4096 bytes")
	}

	for _, character := range path {
		if unicode.IsControl(character) {
			return errors.New("OpenCode runtime home must not contain control characters")
		}
	}

	return nil
}

func reservedOpenCodeEnvKey(key string) bool {
	upper := strings.ToUpper(key)

	return adapterPrivateEnvKey(upper) || managedOpenCodeRootEnvKey(upper)
}

// adapterPrivateEnvKey names the carriers that belong to the adapter and to
// nothing downstream: the private supervisor namespace and the Darwin
// runtime/scratch markers a contained generation stamps onto its own child.
// The comparison is case-insensitive because the ambient environment is not
// case-normalized on every platform, and a variant spelling that survives the
// scrub is the same leak as the canonical one.
func adapterPrivateEnvKey(key string) bool {
	upper := strings.ToUpper(key)

	return strings.HasPrefix(upper, privateAdapterEnvPrefix) ||
		upper == opencode.DarwinRuntimeIDEnv || upper == opencode.DarwinScratchRootEnv
}

func managedOpenCodeRootEnvKey(key string) bool {
	switch strings.ToUpper(key) {
	case "HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", managedEnvXDGDataHome, "XDG_RUNTIME_DIR", "XDG_STATE_HOME",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", managedEnvOpenCodeConfigDir, "OPENCODE_DB":
		return true
	default:
		return false
	}
}

func normalizeStandaloneHome(options *Options) error {
	if options == nil || options.ProcessIsolation == nil {
		return nil
	}

	isolation := options.ProcessIsolation
	if isolation.IdentityLock != nil || isolation.AuthorityDomain != nil || isolation.StandaloneStateRoot == "" {
		return nil
	}

	if options.Home == "" {
		options.Home = isolation.StandaloneStateRoot

		return nil
	}

	if options.Home != isolation.StandaloneStateRoot {
		return errors.New("WithHome must equal ProcessIsolation.StandaloneStateRoot " + isolation.StandaloneStateRoot)
	}

	return nil
}
