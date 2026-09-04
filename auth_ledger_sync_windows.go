//go:build windows

package opencodeacp

// syncLedgerDirectory is a no-op on Windows. FlushFileBuffers does not accept a
// directory handle opened through os.Open and fails the whole ledger write with
// "Access is denied", which would make every authorize commit unreachable. The
// entry itself stays durable: its bytes are flushed with FlushFileBuffers before
// the handle closes, and NTFS records the publishing rename in its own metadata
// log, so a crash leaves either the old entry or the new one and never a torn
// file. Only the directory entry's extra barrier is unavailable.
func syncLedgerDirectory(string) error { return nil }
