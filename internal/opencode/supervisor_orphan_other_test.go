//go:build !linux

package opencode

import "testing"

// preserveProcessSubreaper has nothing to restore here: the child-subreaper
// flag the containment constructors set is a Linux notion.
func preserveProcessSubreaper(*testing.T) {}
