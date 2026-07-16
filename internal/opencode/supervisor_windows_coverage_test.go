//go:build windows

package opencode

import "testing"

// Windows compile-checks the platform-native Job Object sources and the
// platform-independent shutdown coverage tests. The Unix supervisor branch
// carries the behavioral helper tests that use these same seams.
func preserveSupervisorGlobals(t *testing.T) {
	t.Helper()
	oldExecutable := supervisorExecutable
	oldCommand := supervisorExecCommand
	oldRandRead := supervisorRandRead
	oldChmod := supervisorChmod
	oldOpenFile := supervisorOpenFile
	oldEncode := supervisorEncodeConfig
	oldGuardianContainment := supervisorNewGuardianContainment
	oldLivenessContainment := supervisorOpenLivenessContainment
	oldInput := supervisorInput
	oldOutput := supervisorOutput
	oldError := supervisorError
	oldExit := supervisorExit
	oldProcessSnapshot := supervisorProcessSnapshot
	t.Cleanup(func() {
		supervisorExecutable = oldExecutable
		supervisorExecCommand = oldCommand
		supervisorRandRead = oldRandRead
		supervisorChmod = oldChmod
		supervisorOpenFile = oldOpenFile
		supervisorEncodeConfig = oldEncode
		supervisorNewGuardianContainment = oldGuardianContainment
		supervisorOpenLivenessContainment = oldLivenessContainment
		supervisorInput = oldInput
		supervisorOutput = oldOutput
		supervisorError = oldError
		supervisorExit = oldExit
		supervisorProcessSnapshot = oldProcessSnapshot
	})
}
