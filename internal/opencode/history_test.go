package opencode

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeHistory(t *testing.T) {
	t.Parallel()

	rows, err := DecodeHistory(strings.NewReader(`[
  {
    "id": "evt_0",
    "aggregate_id": "ses_a",
    "seq": 0,
    "type": "session.created.1",
    "data": "{\"sessionID\": \"ses_a\", \"info\": {\"title\": \"<b> & </b>\"}}"
  },
  {"id": "evt_1", "aggregate_id": "ses_a", "seq": 1, "type": "session.updated.1", "data": "{\"sessionID\":\"ses_a\"}"}
]`))
	require.NoError(t, err)
	require.Equal(t, []string{
		`{"aggregate_id":"ses_a","data":{"sessionID":"ses_a","info":{"title":"\u003cb\u003e \u0026 \u003c/b\u003e"}},"id":"evt_0","seq":0,"type":"session.created.1"}`,
		`{"aggregate_id":"ses_a","data":{"sessionID":"ses_a"},"id":"evt_1","seq":1,"type":"session.updated.1"}`,
	}, []string{string(rows[0]), string(rows[1])}, "a row's members are in name order and its data compact")

	empty, err := DecodeHistory(strings.NewReader("[]\n"))
	require.NoError(t, err)
	require.Empty(t, empty)

	for name, output := range map[string]string{
		"not an array":    `{}`,
		"truncated":       `[{"id":"evt_0","aggregate_id":"ses_a","seq":0,"type":"t","data":"{}"}`,
		"data not JSON":   `[{"id":"evt_0","aggregate_id":"ses_a","seq":0,"type":"t","data":"{nope"}]`,
		"data not object": `[{"id":"evt_0","aggregate_id":"ses_a","seq":0,"type":"t","data":"[]"}]`,
	} {
		_, err := DecodeHistory(strings.NewReader(output))
		require.Error(t, err, name)
	}
}
