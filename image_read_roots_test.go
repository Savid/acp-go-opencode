package opencodeacp

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestManagedMediaRefusesPreparationDomains(t *testing.T) {
	for _, location := range []string{"scratch", "home", "inside home", "ancestor"} {
		t.Run(location, func(t *testing.T) {
			base := t.TempDir()
			home, scratch := filepath.Join(base, "home"), filepath.Join(base, "scratch")
			handoff := map[string]string{"scratch": scratch, "home": home, "inside home": filepath.Join(home, "images"), "ancestor": base}[location]
			require.NoError(t, os.MkdirAll(handoff, 0o700))
			authority := newAuthorityTrace()
			agent := NewAgent(WithHostAuthority(authority), WithHome(home), WithScratchDir(scratch), WithInputHandoffRoot(handoff))
			t.Cleanup(func() { require.NoError(t, agent.Close()) })
			session := testSession(t, agent, newFakeOpenCodeClient(t))
			session.cwd = handoff
			agent.rememberImageWorkspaces(session.cwd, nil)
			agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
				require.NoError(t, options.PrepareTree(ctx, options.Root))

				return newFakeOpenCodeClient(t), nil
			}
			_, err := agent.startSharedRuntime(t.Context())
			require.NoError(t, err)
			require.NotEmpty(t, authority.prepared)
			// This child did not exist when the read domain was frozen.
			target := home
			if location == "scratch" {
				target = filepath.Join(scratch, "future-native")
			}
			if location == "inside home" {
				target = handoff
			}
			require.NoError(t, os.MkdirAll(target, 0o700))
			png := fixtureImage(t, "valid.png")
			path := writeHandoffFile(t, target, "secret.png", png)
			read := imageReadAll
			imageReadAll = func(io.Reader) ([]byte, error) {
				t.Error("read prepared media")

				return nil, nil
			}
			t.Cleanup(func() { imageReadAll = read })
			requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, handoffEnvelope(png)), imageErrorPathNotAllowed, handoffCauseOutsideRoot)
			_, err = session.materializeLocalImage(path)
			data := assertTurnFailed(t, err, causeTransport, "")
			require.Equal(t, outputReasonPathNotAllowed, data[jsonFieldReason])
		})
	}
}

func TestManagedMediaKeepsDisjointHandlesAcrossRuntimeRetirement(t *testing.T) {
	base := t.TempDir()
	home, scratch, workspace, handoff, extra := filepath.Join(base, "home"), filepath.Join(base, "scratch"), filepath.Join(base, "workspace"), filepath.Join(base, "handoff"), filepath.Join(base, "extra")
	for _, dir := range []string{workspace, handoff, extra} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	png := fixtureImage(t, "valid.png")
	input := writeHandoffFile(t, handoff, "input.png", png)
	output := writeHandoffFile(t, workspace, "output.png", png)
	peerOutput := writeHandoffFile(t, extra, "peer.png", png)
	authority := newAuthorityTrace()
	agent := NewAgent(WithHostAuthority(authority), WithHome(home), WithScratchDir(scratch), WithInputHandoffRoot(handoff))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	node := newFakeOpenCodeClient(t)
	session := &session{agent: agent, cwd: workspace, client: node}
	agent.rememberImageWorkspaces(workspace, []string{extra})
	var starts int
	agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
		starts++
		require.NoError(t, options.PrepareTree(ctx, options.Root))

		return node, nil
	}
	_, generation, err := agent.sharedRuntimeBinding(t.Context())
	require.NoError(t, err)
	pinned := agent.imageRoots.roots[handoff].handle
	// Replacement has different bytes. Both media paths must use their original
	// directory descriptors, even while native ownership remains unresolved.
	for _, dir := range []string{handoff, workspace} {
		require.NoError(t, os.Rename(dir, dir+"-original"))
		require.NoError(t, os.Mkdir(dir, 0o700))
	}
	writeHandoffFile(t, handoff, "input.png", fixtureImage(t, "valid.jpg"))
	writeHandoffFile(t, workspace, "output.png", fixtureImage(t, "valid.jpg"))
	node.closeErr = ErrNativeTreeBusy
	require.ErrorIs(t, agent.retireSharedRuntime(generation, "test retirement"), ErrNativeTreeBusy)
	resolved, err := session.validatePromptMedia(t.Context(), []acp.ContentBlock{handoffBlock(mimePNG, input, handoffEnvelope(png))})
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString(png), resolved[0].data)
	decoded, err := session.materializeLocalImage(output)
	require.NoError(t, err)
	require.Equal(t, png, decoded)
	_, err = session.materializeLocalImage(peerOutput)
	data := assertTurnFailed(t, err, causeTransport, "")
	require.Equal(t, outputReasonPathNotAllowed, data[jsonFieldReason])
	session.additionalDirectories = []string{extra}
	decoded, err = session.materializeLocalImage(peerOutput)
	require.NoError(t, err)
	require.Equal(t, png, decoded)
	derived := filepath.Join(extra, "later-session")
	require.NoError(t, os.Mkdir(derived, 0o700))
	later := newSession(agent, "later", derived, nil, testNativeSession("later"), node, sessionMeta{}, idmapRecord{})
	laterPath := writeHandoffFile(t, derived, "later.png", png)
	decoded, err = later.materializeLocalImage(laterPath)
	require.NoError(t, err)
	require.Equal(t, png, decoded)
	_, err = later.materializeLocalImage(peerOutput)
	data = assertTurnFailed(t, err, causeTransport, "")
	require.Equal(t, outputReasonPathNotAllowed, data[jsonFieldReason])
	node.closeErr = nil
	require.NoError(t, authority.ReclaimNativeTree(t.Context(), home))
	require.NoError(t, agent.retryRuntimeCleanup(t.Context(), agent.runtimeSequencing))
	require.NoError(t, validatePromptMediaError(session, handoffBlock(mimePNG, input, handoffEnvelope(png))))
	require.Equal(t, 1, starts, "handoff after reclaim must precede runtime recovery")
	require.NoError(t, agent.Close())
	_, err = pinned.Stat(".")
	require.Error(t, err, "Agent.Close owns the retained read handles")
}

func TestManagedHandoffPregatesDoNotReserveOrOpenPaths(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	for _, gate := range []string{"envelope", "mime", "size", "cancel"} {
		t.Run(gate, func(t *testing.T) {
			base := t.TempDir()
			home, scratch := filepath.Join(base, "not-created-home"), filepath.Join(base, "not-created-scratch")
			agent := NewAgent(WithHostAuthority(newAuthorityTrace()), WithHome(home), WithScratchDir(scratch), WithInputHandoffRoot(base))
			t.Cleanup(func() { require.NoError(t, agent.Close()) })
			session := testSession(t, agent, newFakeOpenCodeClient(t))
			block := handoffBlock(mimePNG, filepath.Join(base, "not-created.png"), handoffEnvelope(png))
			limits := defaultImageLimits()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch gate {
			case "envelope":
				block.Image.Meta = nil
			case "mime":
				block.Image.MimeType = "image/svg+xml"
			case "size":
				limits.MaxInputBytesPerImage = 1
			case "cancel":
				cancel()
			}
			_, err := session.readHandoffImage(ctx, promptMediaBlocks([]acp.ContentBlock{block})[0], limits)
			require.Error(t, err)
			require.False(t, agent.imageRoots.ready)
			require.NoDirExists(t, home)
			require.NoDirExists(t, scratch)
		})
	}
	agent := NewAgent(WithHostAuthority(newAuthorityTrace()), WithScratchDir(t.TempDir()))
	require.NoError(t, agent.Close())
	require.ErrorIs(t, agent.prepareManagedImageRoots(), errManagedImageRoot)
}

func TestManagedMediaRejectsDirectoryIdentityAlias(t *testing.T) {
	base := t.TempDir()
	home, alias := filepath.Join(base, "home"), filepath.Join(base, "alias")
	require.NoError(t, os.Mkdir(home, 0o700))
	if err := os.Symlink(home, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	png := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, home, "native.png", png)
	agent := NewAgent(WithHostAuthority(newAuthorityTrace()), WithHome(home), WithScratchDir(filepath.Join(base, "scratch")), WithInputHandoffRoot(alias))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	require.NoError(t, agent.prepareManagedImageRoots())
	session := &session{agent: agent, cwd: alias}
	requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, handoffEnvelope(png)), imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	require.Empty(t, agent.imageRoots.roots)
}
