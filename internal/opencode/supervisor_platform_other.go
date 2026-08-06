//go:build !linux

package opencode

import (
	"io"
	"os"
)

func init() {
	configureSupervisorPlatform()
	supervisorBootstrap()
}

// configureSupervisorPlatform installs the identity seams for every platform
// that has no agent identity registry to bind: the supervisor keeps its markers
// under the scratch root it was handed, adopts nothing, and has no trusted root
// identity or guardian peer to prove.
func configureSupervisorPlatform() {
	supervisorMarkerRoot = func(config supervisorConfig) (string, error) { return config.Scratch, nil }
	supervisorAcquireIdentityAuthority = func(
		uint32, uint32, string, string, io.Reader,
	) (supervisorIdentityLock, supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, noopSupervisorIdentityLock{}, nil
	}
	supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
	supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, nil
	}
	supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, nil
	}
	supervisorValidateAdoptedAuthority = func(supervisorConfig) error { return nil }
	supervisorValidateGuardianPeer = func(*os.File, <-chan struct{}) error { return nil }
}
