//go:build unix

package opencodeacp

import "syscall"

// handoffOpenFlags keep the open from blocking. A read root confines where a
// name may resolve but not what kind of file it names, so a FIFO or device node
// placed under it would otherwise park the prompt turn inside open until a
// writer appeared. The descriptor is what refuses it afterwards.
const handoffOpenFlags = syscall.O_NONBLOCK
