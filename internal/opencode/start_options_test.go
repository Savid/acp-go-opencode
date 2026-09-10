package opencode

import "os"

func testStartOptions(options StartOptions) StartOptions {
	if options.NativeEnvironment == nil {
		options.NativeEnvironment = func() map[string]string { return environmentMap(os.Environ()) }
	}

	return options
}
