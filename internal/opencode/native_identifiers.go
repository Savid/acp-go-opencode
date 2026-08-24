package opencode

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// OpenCode builds every identifier as a kind prefix, a 48-bit stamp of the
// millisecond it was minted in plus a per-millisecond counter, and a random
// tail in a base62 alphabet. The server admits a client-supplied message id
// only when it carries the message prefix, and native storage orders messages
// on the stamp, so an id this adapter mints for a prompt is built the way
// OpenCode builds its own.
const (
	messageIDPrefix    = "msg_"
	messageIDAlphabet  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	messageIDTailBytes = 14
	messageIDStampSpan = 4096
	messageIDStampMask = 1<<48 - 1
)

var (
	messageIDClock  = time.Now
	messageIDMu     sync.Mutex
	messageIDMillis int64
	messageIDCount  int64
)

// NewMessageID mints one native user-message identifier from entropy.
func NewMessageID(entropy io.Reader) (string, error) {
	tail := make([]byte, messageIDTailBytes)
	if _, err := io.ReadFull(entropy, tail); err != nil {
		return "", fmt.Errorf("read native message id entropy: %w", err)
	}

	for index, value := range tail {
		tail[index] = messageIDAlphabet[int(value)%len(messageIDAlphabet)]
	}

	return fmt.Sprintf("%s%012x%s", messageIDPrefix, nextMessageIDStamp(), tail), nil
}

// nextMessageIDStamp advances the shared clock stamp. The counter makes two ids
// minted in the same millisecond order by the time they were asked for, and the
// mask keeps the stamp to the 48 bits OpenCode's own identifiers carry.
func nextMessageIDStamp() int64 {
	messageIDMu.Lock()
	defer messageIDMu.Unlock()

	millis := messageIDClock().UnixMilli()
	if millis != messageIDMillis {
		messageIDMillis = millis
		messageIDCount = 0
	}

	messageIDCount++

	return (millis*messageIDStampSpan + messageIDCount) & messageIDStampMask
}
