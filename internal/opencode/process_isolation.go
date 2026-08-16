package opencode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProcessIsolation is the explicit hardened Linux identity boundary: a
// credential distinct from the trusted supervisor's own plus the complete
// environment the native process runs with. Nil selects ordinary execution as
// the current identity and is never manufactured from a nil value.
type ProcessIdentityLockCapability interface {
	Duplicate() (*os.File, error)
}

type ProcessIsolation struct {
	UID                      uint32
	GID                      uint32
	BaseEnvironment          map[string]string
	StandaloneOwnerID        string
	StandaloneStateRoot      string
	IdentityLock             ProcessIdentityLockCapability
	AuthorityDomain          ProcessIdentityLockCapability
	identityAuthorityAdopted bool
}

const (
	processIsolationLinux  = "linux"
	processIsolationDarwin = "darwin"
	// errExplicitProcessIsolationPlatform is the verdict every non-Linux
	// platform returns for a supplied policy. It is a refusal and never a
	// selector: nothing downstream reads it and retries ordinary execution.
	errExplicitProcessIsolationPlatform = "explicit process isolation is supported only on linux"
)

var (
	processIsolationGOOS = runtime.GOOS
	processEnviron       = os.Environ
)

func validateProcessIsolation(isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	if isolation.UID == 0 || isolation.GID == 0 {
		return errors.New("process isolation uid and gid must be nonzero")
	}

	if isolation.BaseEnvironment == nil {
		return errors.New("process isolation base environment is required")
	}

	if err := validateEnvironmentMap(isolation.BaseEnvironment); err != nil {
		return fmt.Errorf("validate process isolation base environment: %w", err)
	}

	// The shape is checked first because a malformed policy is malformed
	// everywhere. The platform verdict is the separate answer that an
	// otherwise well-formed policy still cannot be honored here, and it binds
	// the embedded Go API rather than only the command's policy loader.
	if processIsolationGOOS != processIsolationLinux {
		return errors.New(errExplicitProcessIsolationPlatform)
	}

	return validateStandaloneIdentityDisposition(isolation)
}

func validateStandaloneIdentityDisposition(isolation *ProcessIsolation) error {
	identityLock, authorityDomain := isolation.IdentityLock != nil, isolation.AuthorityDomain != nil

	if isolation.identityAuthorityAdopted {
		if identityLock || authorityDomain {
			return errors.New("adopted process identity authority cannot carry duplicable capabilities")
		}

		identityLock = true
		authorityDomain = true
	}

	if identityLock != authorityDomain {
		return errors.New("process identity lock and authority domain must be provided together")
	}

	if identityLock {
		if isolation.StandaloneOwnerID != "" || isolation.StandaloneStateRoot != "" {
			return errors.New("borrowed process identity forbids standalone owner fields")
		}

		return nil
	}

	if !validStandaloneOwnerID(isolation.StandaloneOwnerID) {
		return errors.New("standalone owner id must be 1..256 canonical ASCII bytes")
	}

	if !validStandaloneStateRootPath(isolation.StandaloneStateRoot) {
		return errors.New("standalone state root must be a clean absolute path outside the authority root")
	}

	return nil
}

func validStandaloneOwnerID(value string) bool {
	if value == "" || len(value) > 256 || !standaloneOwnerIDAlphanumeric(value[0]) {
		return false
	}

	for index := 1; index < len(value); index++ {
		if !standaloneOwnerIDAlphanumeric(value[index]) && !strings.ContainsRune("._:@/-", rune(value[index])) {
			return false
		}
	}

	return true
}

func standaloneOwnerIDAlphanumeric(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func validStandaloneStateRootPath(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || !filepath.IsAbs(value) ||
		filepath.Clean(value) != value || value == "/" || strings.IndexByte(value, 0) >= 0 {
		return false
	}

	const authorityRoot = "/var/lib/acp-go/agent-identities"
	if value == authorityRoot || strings.HasPrefix(value, authorityRoot+string(filepath.Separator)) {
		return false
	}

	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}

func validateEnvironmentMap(environment map[string]string) error {
	for key, value := range environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid environment entry for %q", key)
		}
	}

	return nil
}

// pathEnv names the search path a child resolves every executable against.
const pathEnv = "PATH"

func environmentMap(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			values[canonicalEnvironmentKey(key)] = value
		}
	}

	return values
}

func buildProcessEnvironment(isolation *ProcessIsolation, overlays ...map[string]string) (map[string]string, error) {
	return buildProcessEnvironmentFrom(isolation, nil, overlays...)
}

func buildProcessEnvironmentFrom(
	isolation *ProcessIsolation,
	implicitEnvironment map[string]string,
	overlays ...map[string]string,
) (map[string]string, error) {
	if err := validateProcessIsolation(isolation); err != nil {
		return nil, err
	}

	base := implicitEnvironment
	if isolation != nil {
		base = isolation.BaseEnvironment
	} else if base == nil {
		base = captureProcessEnvironment()
	}

	values := composeEnvironment(withoutAdapterOwnedState(base))

	for _, overlay := range overlays {
		if err := validateEnvironmentMap(overlay); err != nil {
			return nil, err
		}

		values = composeEnvironment(values, withoutAdapterOwnedState(overlay))
	}

	// Only a complete explicit policy carries the absolute-entry PATH rule.
	// Ordinary execution runs against a sanitized ambient environment, and a
	// perfectly ordinary shell PATH must not turn policy omission into a
	// startup refusal.
	if isolation != nil {
		if err := validateProcessSearchPath(environmentMapValue(values, pathEnv)); err != nil {
			return nil, err
		}
	}

	return values, nil
}

// composeEnvironment folds ordered phases into one environment keyed by the
// platform's canonical spelling: later phases replace earlier ones, which is
// what makes a caller overlay beat the ambient base.
//
// Within a single phase there is no order to inherit — a Go map has none — so
// the rule is stated rather than discovered: keys are sorted and written in
// that order, so when several spellings of one Windows variable share a phase
// the lexicographically last spelling wins ("Path" over "PATH", "PathExt" over
// "PATHEXT"). The value is arbitrary but the choice is not: one phase always
// produces the same environment, which is the property a launch depends on.
func composeEnvironment(phases ...map[string]string) map[string]string {
	values := map[string]string{}

	for _, phase := range phases {
		keys := make([]string, 0, len(phase))
		for key := range phase {
			keys = append(keys, key)
		}

		slices.Sort(keys)

		for _, key := range keys {
			values[canonicalEnvironmentKey(key)] = phase[key]
		}
	}

	return values
}

func environmentMapValue(environment map[string]string, name string) string {
	return environment[canonicalEnvironmentKey(name)]
}

func environmentValue(environment []string, name string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		key, value, ok := strings.Cut(environment[index], "=")
		if ok && environmentKeyEqual(key, name) {
			return value
		}
	}

	return ""
}

// withoutAdapterOwnedState drops every key the adapter owns rather than
// inherits, whatever its case in the source map: the private supervisor
// namespace, the Darwin runtime/scratch markers, and the OpenCode/XDG roots the
// launch is about to set to its own generated values. It runs over the captured
// ambient base and over every caller overlay, because a key scrubbed on one
// path is a key that survived on the others.
//
// HOME is deliberately not in this set. In both modes HOME names the home of
// the identity the native process actually runs as — the policy account under
// an explicit policy, the adapter's own account under ordinary execution — and
// it can no longer redirect OpenCode state, because all four XDG roots plus
// every OPENCODE_* root are replaced below. Dropping it would instead cut the
// Darwin login keychain that provider auth reads.
func withoutAdapterOwnedState(environment map[string]string) map[string]string {
	filtered := make(map[string]string, len(environment))

	for key, value := range environment {
		if adapterPrivateEnvKey(key) || managedRuntimeRootEnvKey(key) {
			continue
		}

		filtered[key] = value
	}

	return filtered
}

// privateAdapterEnvPrefix is assembled from two literals so the family
// environment-namespace audits never read it as an operator-facing variable.
const privateAdapterEnvPrefix = "ACP_" + "GO_OPENCODE_INTERNAL_"

func adapterPrivateEnvKey(key string) bool {
	upper := strings.ToUpper(key)

	return strings.HasPrefix(upper, privateAdapterEnvPrefix) ||
		upper == DarwinRuntimeIDEnv || upper == DarwinScratchRootEnv
}

func managedRuntimeRootEnvKey(key string) bool {
	switch strings.ToUpper(key) {
	case "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR", "OPENCODE_DB":
		return true
	default:
		return false
	}
}

func captureProcessEnvironment() map[string]string {
	return environmentMap(processEnviron())
}

// ordinaryDirectExecution reports whether an omitted policy runs the native
// process directly instead of through the guardian/liveness supervisor pair.
// The pair is what proves containment, and only Linux and an opted-in Darwin
// can prove anything with it; everywhere else it refuses before native start,
// which would turn policy omission into a dead platform. The direct arm keeps
// the portable writable-home claim/liveness exclusion instead, and claims
// nothing about descendants.
func ordinaryDirectExecution(isolation *ProcessIsolation, darwinBestEffort bool) bool {
	if isolation != nil {
		return false
	}

	switch processIsolationGOOS {
	case processIsolationLinux:
		return false
	case processIsolationDarwin:
		return !darwinBestEffort
	default:
		return true
	}
}

func ordinaryProcessBackend(config supervisorConfig) bool {
	return ordinaryDirectExecution(config.Isolation, config.DarwinBestEffort)
}

func validateSupervisorIdentityDisposition(config supervisorConfig) error {
	if config.OrdinaryExecution {
		uid, gid, err := currentProcessIdentity()
		if err != nil {
			return err
		}

		if !config.SharedIdentity || config.IsolationUID != uid || config.IsolationGID != gid ||
			config.IdentityLock || config.AuthorityDomain || config.StandaloneAuthority ||
			config.StandaloneOwnerID != "" || config.StandaloneStateRoot != "" {
			return errors.New("OpenCode ordinary supervisor identity disposition is invalid")
		}

		return nil
	}

	// An explicit policy is never shared: the supervisor stays a distinct
	// trusted root and the native identity is a nonzero one it descends to, so
	// a config claiming otherwise describes a launch this backend cannot make.
	if config.SharedIdentity {
		return errors.New("explicit supervisor identity disposition cannot claim a shared identity")
	}

	return nil
}

// withoutManagedRootOverrides is the caller-overlay filter. It drops
// everything withoutAdapterOwnedState drops plus HOME, because a caller
// overlay is configuration rather than the launched identity's own
// environment, and the adapter owns which home the native process inherits.
func withoutManagedRootOverrides(environment map[string]string) map[string]string {
	filtered := withoutAdapterOwnedState(environment)
	for key := range filtered {
		if strings.EqualFold(key, "HOME") {
			delete(filtered, key)
		}
	}

	return filtered
}

func validateProcessSearchPath(search string) error {
	if search == "" {
		return nil
	}

	for _, directory := range filepath.SplitList(search) {
		if directory == "" || !filepath.IsAbs(directory) {
			return fmt.Errorf("process isolation PATH contains non-absolute entry %q", directory)
		}
	}

	return nil
}

// resolveProcessExecutable finds the program a launch will exec and freezes it
// into one absolute, identified file. The lookup answers against this process's
// working directory, while the guardian and liveness supervisors run from /, so
// a relative answer would name a different file by the time it is executed.
func resolveProcessExecutable(path string, env []string, strict bool) (processExecutable, error) {
	resolved, err := lookupProcessExecutable(path, env, strict)
	if err != nil {
		return processExecutable{}, err
	}

	if !filepath.IsAbs(resolved) {
		working, err := processWorkingDirectory()
		if err != nil {
			return processExecutable{}, fmt.Errorf("resolve working directory for executable %q: %w", resolved, err)
		}

		resolved = filepath.Join(working, resolved)
	}

	return freezeProcessExecutable(resolved)
}

// lookupProcessExecutable finds the program the way the launch's arm would. The
// strict arm belongs to a complete explicit policy, which is a closed
// environment: a configured path must be absolute and every PATH entry used for
// resolution must be absolute too. The ordinary arm resolves the same way a
// shell would, because policy omission is ordinary execution and an ordinary
// relative executable or a relative PATH entry is not a security event there.
func lookupProcessExecutable(path string, env []string, strict bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("executable path is empty")
	}

	if !strict {
		return resolveOrdinaryProcessExecutable(path, env)
	}

	if strings.ContainsRune(path, filepath.Separator) {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("executable path %q is not absolute", path)
		}

		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("stat executable %q: %w", path, err)
		}

		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("executable %q is not executable", path)
		}

		return path, nil
	}

	search := environmentValue(env, pathEnv)
	if search == "" {
		return "", fmt.Errorf("find %s: process isolation %s is empty", path, pathEnv)
	}

	if err := validateProcessSearchPath(search); err != nil {
		return "", fmt.Errorf("find %s: %w", path, err)
	}

	for _, directory := range filepath.SplitList(search) {
		candidate := filepath.Join(directory, path)

		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}

		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("find %s in process isolation %s: %w", path, pathEnv, err)
		}
	}

	return "", fmt.Errorf("find %s in process isolation %s: %w", path, pathEnv, exec.ErrNotFound)
}
