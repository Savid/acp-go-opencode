//go:build windows

package opencodeacp

// syncRestoreOwnershipDirectory is a no-op on Windows. FlushFileBuffers does not
// accept a directory handle opened through os.Open, so flushing the control root
// fails with "Access is denied" and takes every restore — and so every prompt
// that replays one — down with it. The registry itself stays durable: the
// temporary file is flushed with FlushFileBuffers before it closes, and NTFS
// records the publishing rename in its own metadata log, so a crash leaves
// either the previous registry or the new one. Only the directory entry's extra
// barrier is unavailable.
func syncRestoreOwnershipDirectory(string) error { return nil }
