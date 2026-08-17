//go:build linux

package opencode

func init() {
	configureSupervisorPlatform()
	supervisorBootstrap()
}

func configureSupervisorPlatform() {
	supervisorWriteConfig = writeLinuxSupervisorConfig
	supervisorMarkerRoot = linuxSupervisorMarkerRoot
	supervisorAcquireIdentityAuthority = acquireLinuxAgentIdentityAuthority
	supervisorVerifyTrustedIdentity = verifyLinuxTrustedSupervisorIdentity
	supervisorAdoptIdentityLock = adoptLinuxAgentIdentityLock
	supervisorAdoptAuthorityDomain = adoptLinuxAgentAuthorityDomain
	supervisorValidateAdoptedAuthority = validateLinuxSupervisorAdoptedAuthority
	supervisorQuarantineRetry = retryLinuxLivenessContainment
	supervisorGuardianQuarantineRetry = retryLinuxGuardianContainment
	supervisorValidateGuardianPeer = validateLinuxSupervisorGuardianPeer
}
