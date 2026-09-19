package opencode

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadAddress(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "native address after noise", output: "startup notice\nopencode server listening on http://127.0.0.1:49152\n", want: "http://127.0.0.1:49152"},
		{name: "missing address", output: "startup failed\n"},
		{name: "unbound port", output: "opencode server listening on http://127.0.0.1:0\n"},
		{name: "foreign address", output: "opencode server listening on http://192.0.2.1:49152\n"},
		{name: "URL credentials", output: "opencode server listening on http://user:password@127.0.0.1:49152\n"},
		{name: "oversized line", output: strings.Repeat("x", 64<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := NewClient()
			err := client.ReadAddress(strings.NewReader(tc.output))
			if tc.want == "" {
				require.Error(t, err)
				require.Empty(t, client.URL)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, client.URL)
			}
		})
	}
}
