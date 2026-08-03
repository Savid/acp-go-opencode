package opencode

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrependPathDirs(t *testing.T) {
	separator := string(os.PathListSeparator)

	tests := []struct {
		name string
		env  []string
		dirs []string
		want []string
	}{
		{
			name: "no directories leaves the environment untouched",
			env:  []string{"PATH=/usr/bin", "HOME=/home/user"},
			want: []string{"PATH=/usr/bin", "HOME=/home/user"},
		},
		{
			name: "directories precede the inherited search path in order",
			env:  []string{"HOME=/home/user", "PATH=/usr/bin" + separator + "/bin"},
			dirs: []string{"/session/bin", "/tools/bin"},
			want: []string{
				"HOME=/home/user",
				"PATH=/session/bin" + separator + "/tools/bin" + separator + "/usr/bin" + separator + "/bin",
			},
		},
		{
			name: "an absent search path becomes the directories alone",
			env:  []string{"HOME=/home/user"},
			dirs: []string{"/session/bin"},
			want: []string{"HOME=/home/user", "PATH=/session/bin"},
		},
		{
			name: "an empty search path is not carried as a trailing entry",
			env:  []string{"PATH="},
			dirs: []string{"/session/bin"},
			want: []string{"PATH=/session/bin"},
		},
		{
			name: "an entry without a separator survives",
			env:  []string{"malformed", "PATH=/usr/bin"},
			dirs: []string{"/session/bin"},
			want: []string{"malformed", "PATH=/session/bin" + separator + "/usr/bin"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, prependPathDirs(test.env, test.dirs))
		})
	}
}

// TestBrowserShimOutranksExtraPathDirs pins the launcher shadow ahead of the
// caller's directories: a shadowed launcher the caller also ships must still
// resolve to the no-op.
func TestBrowserShimOutranksExtraPathDirs(t *testing.T) {
	env := prependPathDirs([]string{"PATH=/usr/bin"}, []string{"/session/bin"})
	env = browserShimEnviron(env, "/shim")

	require.Contains(t, env, "PATH=/shim"+string(os.PathListSeparator)+
		"/session/bin"+string(os.PathListSeparator)+"/usr/bin")
}
