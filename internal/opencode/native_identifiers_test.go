package opencode

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// nativeMessageIDShape is the spelling the installed OpenCode server gives its
// own message identifiers, read off the ids it wrote to its journal.
const nativeMessageIDShape = `^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`

type failingEntropy struct{}

func (failingEntropy) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

// pinMessageIDClock fixes the mint clock and clears the counter it shares, so a
// stamp a test asserts on is the stamp that millisecond's first id carries.
func pinMessageIDClock(t *testing.T, at time.Time) {
	t.Helper()

	previous := messageIDClock
	messageIDClock = func() time.Time { return at }

	messageIDMu.Lock()
	messageIDMillis, messageIDCount = 0, 0
	messageIDMu.Unlock()

	t.Cleanup(func() { messageIDClock = previous })
}

// TestNewMessageIDMintsOpenCodeIdentifierShape drives the mint against the three
// real ids the installed server journalled, and against the message they belong
// to: `msg_022cba89d001cpfIW4PKqfWORF` was created at 1787290167453, which is
// exactly the stamp this mint writes for the first id of that millisecond.
func TestNewMessageIDMintsOpenCodeIdentifierShape(t *testing.T) {
	for _, native := range []string{
		"msg_022cba89d001cpfIW4PKqfWORF",
		"msg_022a15a7c001q8N0SrbIrAAOK0",
		"msg_0229ee7be0019piFckrxCGSH3j",
	} {
		require.Regexp(t, nativeMessageIDShape, native)
	}

	pinMessageIDClock(t, time.UnixMilli(1787290167453))

	id, err := NewMessageID(bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}))
	require.NoError(t, err)
	require.Regexp(t, nativeMessageIDShape, id)
	require.Equal(t, "msg_022cba89d0010123456789ABCD", id)
	require.Len(t, id, len("msg_022cba89d001cpfIW4PKqfWORF"))
}

// TestNewMessageIDStampsCreationOrder proves the stamp keeps ids sortable the
// way native storage expects: the counter separates two ids minted in one
// millisecond, and a later millisecond starts counting again.
func TestNewMessageIDStampsCreationOrder(t *testing.T) {
	minted := time.UnixMilli(1787290167453)
	pinMessageIDClock(t, minted)

	entropy := strings.NewReader(strings.Repeat("z", 3*messageIDTailBytes))

	first, err := NewMessageID(entropy)
	require.NoError(t, err)

	second, err := NewMessageID(entropy)
	require.NoError(t, err)

	require.Equal(t, "022cba89d001", first[len(messageIDPrefix):len(messageIDPrefix)+12])
	require.Equal(t, "022cba89d002", second[len(messageIDPrefix):len(messageIDPrefix)+12])
	require.Less(t, first, second)

	messageIDClock = func() time.Time { return minted.Add(time.Millisecond) }

	third, err := NewMessageID(entropy)
	require.NoError(t, err)
	require.Equal(t, "022cba89e001", third[len(messageIDPrefix):len(messageIDPrefix)+12])
	require.Less(t, second, third)
}

// TestNewMessageIDRefusesWithoutEntropy proves an id is never minted from a
// short read: an identifier with a truncated tail would still be admitted by the
// server, so the mint itself has to fail.
func TestNewMessageIDRefusesWithoutEntropy(t *testing.T) {
	_, err := NewMessageID(failingEntropy{})
	require.ErrorContains(t, err, "read native message id entropy")
}
