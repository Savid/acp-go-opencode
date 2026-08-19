//go:build windows

package opencodeacp

// handoffOpenFlags adds nothing on Windows. The Unix flag exists to keep the
// open of a FIFO or device node from parking the prompt turn until a peer
// appears, and Windows offers no such flag to ask for: an open there does not
// block on the file kinds a read root can hold. Nothing about the gate weakens
// for it — the root still decides where a name may resolve, and the descriptor's
// own kind check still refuses anything that is not a regular file.
const handoffOpenFlags = 0
