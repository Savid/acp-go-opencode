//go:build linux

package opencode

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// brokerLeaseProcStatFields returns a plausible tail for a /proc stat line: the
// fields that follow the executable name, with the start time in the position
// the kernel puts it. Building it rather than pasting a literal keeps the count
// under the test's own control, which is the whole point of the second case.
func brokerLeaseProcStatFields(count int, startTime string) []string {
	fields := make([]string, 0, count)
	for index := range count {
		fields = append(fields, strconv.Itoa(index))
	}

	if len(fields) > 19 {
		fields[19] = startTime
	}

	return fields
}

// brokerLeaseProcStatLine assembles a /proc stat line the way the kernel does:
// the pid, the executable name in parentheses, then the numbered fields.
func brokerLeaseProcStatLine(comm string, fields []string) string {
	return "4242 (" + comm + ") " + strings.Join(fields, " ") + "\n"
}

// brokerLeaseFaultProcStat makes every /proc stat read answer with contents,
// which is the only way to present a stat line the running kernel would never
// write.
func brokerLeaseFaultProcStat(t *testing.T, contents string) {
	t.Helper()

	previous := leaseProcReadFile
	leaseProcReadFile = func(string) ([]byte, error) { return []byte(contents), nil }

	t.Cleanup(func() { leaseProcReadFile = previous })
}

// TestProcessStartTimeReadsFieldTwentyTwoAfterTheExecutableName proves the scan
// really does start after the *last* closing parenthesis. A process may name
// itself with spaces and parentheses, and splitting the whole line on
// whitespace would then read some fragment of the name as the start time — the
// one value the reaper uses to tell the process a lease named from an unrelated
// process that inherited its PID.
func TestProcessStartTimeReadsFieldTwentyTwoAfterTheExecutableName(t *testing.T) {
	brokerLeaseFaultProcStat(
		t, brokerLeaseProcStatLine("opencode (server) tui", brokerLeaseProcStatFields(28, "998877")),
	)

	start, err := processStartTime(os.Getpid())
	require.NoError(t, err)
	require.Equal(t, "998877", start)
}

// TestProcessStartTimeRefusesAStatLineItCannotIdentifyAProcessFrom proves a
// /proc entry that cannot yield field 22 is refused outright instead of
// answering with whatever happens to be in reach. Both shapes matter: a line
// with no closing parenthesis has no anchor to count from, and a truncated line
// has an anchor but not enough fields behind it. Either way the reaper must be
// told it has no identity for the PID rather than be handed one that would let
// it kill an innocent process, so each case asserts the empty answer as well as
// the refusal.
func TestProcessStartTimeRefusesAStatLineItCannotIdentifyAProcessFrom(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		contents string
	}{
		{
			name:     "no closing parenthesis to count from",
			contents: "4242 opencode " + strings.Join(brokerLeaseProcStatFields(28, "998877"), " "),
		},
		{
			name:     "too few fields behind the executable name",
			contents: brokerLeaseProcStatLine("opencode", brokerLeaseProcStatFields(19, "")),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			brokerLeaseFaultProcStat(t, testCase.contents)

			start, err := processStartTime(os.Getpid())
			require.ErrorIs(t, err, errMalformedProcStat)
			require.Empty(t, start, "a stat line it could not parse still produced an identity")
		})
	}
}
