package opencodeacp

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestOutputHelperPredicates(t *testing.T) {
	t.Run("parseImageDataURL", func(t *testing.T) {
		_, _, _, ok := parseImageDataURL("not-a-data-url")
		require.False(t, ok)
		_, _, _, ok = parseImageDataURL("data:image/png;base64")
		require.False(t, ok)
		_, _, _, ok = parseImageDataURL("data:image/png,plain")
		require.False(t, ok)

		prefix, mime, payload, ok := parseImageDataURL("data:image/png;charset=utf8;base64,QUJD")
		require.True(t, ok)
		require.Equal(t, "data:image/png;charset=utf8;base64,", prefix)
		require.Equal(t, mimePNG, mime)
		require.Equal(t, "QUJD", payload)
	})

	t.Run("isImageMIME", func(t *testing.T) {
		require.True(t, isImageMediaType("  IMAGE/PNG  "))
		require.False(t, isImageMediaType("text/plain"))
	})

	t.Run("remoteArtifactURL", func(t *testing.T) {
		require.True(t, remoteArtifactURL("http://example.com/a.png"))
		require.True(t, remoteArtifactURL("https://example.com/a.png"))
		require.False(t, remoteArtifactURL("file:///tmp/a.png"))
		require.False(t, remoteArtifactURL("data:image/png;base64,AA=="))
		require.False(t, remoteArtifactURL("%zz"))
	})

	t.Run("localArtifactPath", func(t *testing.T) {
		require.Equal(t, "/abs/a.png", localArtifactPath("/abs/a.png"))
		require.Equal(t, "/tmp/a.png", localArtifactPath("file:///tmp/a.png"))
		require.Empty(t, localArtifactPath("http://example.com/a.png"))
		require.Empty(t, localArtifactPath("%zz"))
	})

	t.Run("resourceLinkItem", func(t *testing.T) {
		withName := resourceLinkItem(opencode.NativeAttachment{URL: "https://x/img.png", Filename: "shot.png", Mime: mimePNG})
		require.Equal(t, "shot.png", withName.block.ResourceLink.Name)
		require.NotNil(t, withName.block.ResourceLink.MimeType)
		require.Equal(t, "link:https://x/img.png", withName.key())

		fromURI := resourceLinkItem(opencode.NativeAttachment{URL: "https://x/pic.png"})
		require.Equal(t, "pic.png", fromURI.block.ResourceLink.Name)

		fromURL := resourceLinkItem(opencode.NativeAttachment{URL: "https://x/"})
		require.Equal(t, "https://x/", fromURL.block.ResourceLink.Name)
	})

	t.Run("toolAttachmentIdentity and content", func(t *testing.T) {
		part := opencode.NativePart{CallID: "call-1"}
		require.Equal(t, "call-1/att-1", toolAttachmentIdentity(part, opencode.NativeAttachment{ID: "att-1"}, 0))
		require.Equal(t, "call-1/2", toolAttachmentIdentity(part, opencode.NativeAttachment{}, 2))

		content := toolCallContentFromItems([]imageOutputItem{{block: acp.ImageBlock("AA==", mimePNG)}})
		require.Len(t, content, 1)
	})
}

func TestMapOutputArtifactNormalization(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")

	t.Run("inline data url image", func(t *testing.T) {
		session, _ := newImageSession(t)
		item, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{
			Mime: mimePNG, URL: dataURL(mimePNG, png),
		}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.True(t, mapped)
		require.True(t, item.isImage)
		require.Equal(t, mimePNG, item.block.Image.MimeType)
		require.Equal(t, int64(len(png)), item.sizeBytes)
	})

	t.Run("data url non-image is skipped", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{
			Mime: "text/plain", URL: "data:text/plain;base64,QUJD",
		}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.False(t, mapped)
	})

	t.Run("remote url image becomes resource link", func(t *testing.T) {
		session, _ := newImageSession(t)
		item, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{
			Mime: mimePNG, URL: "https://example.com/generated.png",
		}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.True(t, mapped)
		require.False(t, item.isImage)
		require.NotNil(t, item.block.ResourceLink)
	})

	t.Run("missing location on image fails", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, _, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, imageOutputStage, data[jsonFieldStage])
		require.Equal(t, outputReasonMissingFile, data[jsonFieldReason])
	})

	t.Run("missing location on non-image is skipped", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: "text/plain"}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.False(t, mapped)
	})

	t.Run("unusable location on image fails", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, _, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: "mailto:x@y.z"}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonMissingFile, data[jsonFieldReason])
	})

	t.Run("unusable location on non-image is skipped", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: "text/plain", URL: "mailto:x@y.z"}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.False(t, mapped)
	})

	t.Run("malformed data url", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, _, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: "data:image/png,plain"}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonInvalidBase64, data[jsonFieldReason])
	})

	t.Run("data url bad base64", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, _, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: "data:image/png;base64,!!!!"}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonInvalidBase64, data[jsonFieldReason])
	})

	t.Run("not a raster", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, _, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: dataURL(mimePNG, []byte("not an image"))}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonNotARaster, data[jsonFieldReason])
	})

	t.Run("declared mime disagrees with bytes", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, _, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: dataURL(mimePNG, fixtureImage(t, "mismatch.png"))}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonMediaTypeMismatch, data[jsonFieldReason])
	})

	t.Run("output is not format allowlisted", func(t *testing.T) {
		session, _ := newImageSession(t)
		bmp := append([]byte("BM"), make([]byte, 40)...)
		item, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimeBMP, URL: dataURL(mimeBMP, bmp)}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.True(t, mapped)
		require.Equal(t, mimeBMP, item.block.Image.MimeType)
	})

	t.Run("per image output limit", func(t *testing.T) {
		session, _ := newImageSession(t)
		session.agent.options.ImageLimits = ImageLimits{MaxOutputBytesPerImage: 1}
		_, _, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: dataURL(mimePNG, png)}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonTooLarge, data[jsonFieldReason])
		require.Equal(t, int64(len(png)), data[jsonFieldSizeBytes])
		require.Equal(t, int64(1), data[jsonFieldMaxBytes])
	})
}

func TestMapOutputArtifactExtraBranches(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")

	t.Run("non-image local path skipped", func(t *testing.T) {
		sess, _ := newImageSession(t)
		path := filepath.Join(sess.cwd, "note.txt")
		require.NoError(t, os.WriteFile(path, []byte("hi"), 0o600))
		_, mapped, err := sess.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: "text/plain", URL: "file://" + path}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.False(t, mapped)
	})

	t.Run("empty data url mime falls back to attachment mime", func(t *testing.T) {
		sess, _ := newImageSession(t)
		item, mapped, err := sess.mapOutputArtifact(ctx, opencode.NativeAttachment{
			Mime: mimePNG, URL: "data:;base64," + base64.StdEncoding.EncodeToString(png),
		}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.True(t, mapped)
		require.Equal(t, mimePNG, item.block.Image.MimeType)
	})

	t.Run("local non-raster file fails at finish", func(t *testing.T) {
		sess, _ := newImageSession(t)
		path := filepath.Join(sess.cwd, "fake.png")
		require.NoError(t, os.WriteFile(path, []byte("still not an image"), 0o600))
		_, _, err := sess.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: "file://" + path}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonNotARaster, data[jsonFieldReason])
	})

	t.Run("register failure inside finish", func(t *testing.T) {
		sess, _ := newImageSession(t)
		original := imageJSONMarshal
		imageJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal boom") }
		t.Cleanup(func() { imageJSONMarshal = original })
		_, _, err := sess.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: dataURL(mimePNG, png)}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
	})
}

func TestMapLocalImageArtifactReplay(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")

	t.Run("replay reuses the stored record", func(t *testing.T) {
		sess, _ := newImageSession(t)
		path := filepath.Join(sess.cwd, "shot.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))
		attachment := opencode.NativeAttachment{Mime: mimePNG, URL: "file://" + path}

		_, mapped, err := sess.mapOutputArtifact(ctx, attachment, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.True(t, mapped)

		require.NoError(t, os.Remove(path))

		item, mapped, err := sess.mapOutputArtifact(ctx, attachment, "id-1", provenanceTool, true)
		require.NoError(t, err)
		require.True(t, mapped)
		require.Equal(t, base64.StdEncoding.EncodeToString(png), item.block.Image.Data)
	})

	t.Run("replay of a swept artifact fails", func(t *testing.T) {
		sess, _ := newImageSession(t)
		_, _, err := sess.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: "file:///tmp/gone.png"}, "missing", provenanceTool, true)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
	})

	t.Run("replay of a corrupt record fails integrity", func(t *testing.T) {
		sess, _ := newImageSession(t)
		sess.setImageArtifacts(map[string]imageArtifactRecord{
			"fp": {Version: imageArtifactRecordVersion, NativeID: "id-bad", Fingerprint: "fp", Mime: mimePNG, Data: "!!!!"},
		})
		_, _, err := sess.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: "file:///tmp/x.png"}, "id-bad", provenanceTool, true)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
	})
}

// TestImageOutputGuidanceSplitsRecoverableFromFatal pins the blast radius of
// every image-output verdict: an ordinary mistake that can be retried carries
// guidance and keeps the turn, the adapter's own store breaking does not.
func TestImageOutputGuidanceSplitsRecoverableFromFatal(t *testing.T) {
	recoverable := map[string]string{
		outputReasonPathNotAllowed:    imageGuidancePathNotAllowed,
		outputReasonMissingFile:       imageGuidanceMissingFile,
		outputReasonTooLarge:          imageGuidanceTooLarge,
		outputReasonNotARaster:        imageGuidanceNotRaster,
		outputReasonInvalidBase64:     imageGuidanceInvalidBase64,
		outputReasonMediaTypeMismatch: imageGuidanceMIMEMismatched,
	}

	for reason, guidance := range recoverable {
		message, ok := imageOutputGuidance(imageOutputFailure(reason, "detail", 0, 0))
		require.True(t, ok, reason)
		require.Equal(t, guidance, message)

		// The guidance says what to do next and never describes the input.
		require.NotContains(t, message, "root")
		require.NotContains(t, message, "path")
	}

	_, ok := imageOutputGuidance(imageOutputFailure(outputReasonStorageFailed, "detail", 0, 0))
	require.False(t, ok)

	_, ok = imageOutputGuidance(acp.NewInternalError(map[string]any{jsonFieldStage: "other"}))
	require.False(t, ok)

	_, ok = imageOutputGuidance(errors.New("not a request error"))
	require.False(t, ok)
}

func TestMaterializeLocalImage(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")

	t.Run("reads an allowed regular file", func(t *testing.T) {
		session, _ := newImageSession(t)
		path := filepath.Join(session.cwd, "shot.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))

		item, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{
			Mime: mimePNG, URL: "file://" + path,
		}, "id-1", provenanceTool, false)
		require.NoError(t, err)
		require.True(t, mapped)
		require.Equal(t, base64.StdEncoding.EncodeToString(png), item.block.Image.Data)
	})

	t.Run("reads the operating-system temp directory", func(t *testing.T) {
		session, _ := newImageSession(t)

		// A configured scratch dir moves the scratch parent off the temp
		// directory, so the temp directory is a root here on its own account.
		WithScratchDir(t.TempDir())(&session.agent.options)

		// The real os.TempDir, not a narrowed one, and reached through
		// os.MkdirTemp so the fixture sits wherever this platform actually puts
		// temp files, symlinked parents included.
		dir, err := os.MkdirTemp("", "opencode-image-output")
		require.NoError(t, err)

		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		path := filepath.Join(dir, "frame_01.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))
		require.False(t, pathWithinRoot(session.cwd, path))

		item, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{
			Mime: mimePNG, URL: "file://" + path,
		}, "id-temp", provenanceTool, false)
		require.NoError(t, err)
		require.True(t, mapped)
		require.Equal(t, base64.StdEncoding.EncodeToString(png), item.block.Image.Data)
	})

	t.Run("reads a temp directory reached through a symlink", func(t *testing.T) {
		session, _ := newImageSession(t)

		base := t.TempDir()
		scratch := filepath.Join(base, "scratch")
		WithScratchDir(scratch)(&session.agent.options)
		require.NoError(t, os.Mkdir(scratch, 0o700))

		// The temp directory is a symlink on macOS, so the root has to be
		// resolved to the same degree as the candidate or it never matches.
		// Narrowing os.TempDir to a link built here puts that on every host
		// rather than only on the hosts whose temp directory happens to be one.
		target := filepath.Join(base, "tmp-target")
		require.NoError(t, os.Mkdir(target, 0o700))

		tempRoot := filepath.Join(base, "tmp")
		require.NoError(t, os.Symlink(target, tempRoot))

		for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
			t.Setenv(name, tempRoot)
		}

		require.Equal(t, tempRoot, os.TempDir())

		path := filepath.Join(tempRoot, "frame_01.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))

		item, mapped, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{
			Mime: mimePNG, URL: "file://" + path,
		}, "id-temp-link", provenanceTool, false)
		require.NoError(t, err)
		require.True(t, mapped)
		require.Equal(t, base64.StdEncoding.EncodeToString(png), item.block.Image.Data)
	})

	t.Run("path outside allowed roots", func(t *testing.T) {
		session, _ := newImageSession(t)
		outside := filepath.Join(narrowedOutsideRoot(t, session), "elsewhere.png")
		require.NoError(t, os.WriteFile(outside, png, 0o600))

		_, _, err := session.mapOutputArtifact(ctx, opencode.NativeAttachment{Mime: mimePNG, URL: "file://" + outside}, "id-1", provenanceTool, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonPathNotAllowed, data[jsonFieldReason])
	})

	t.Run("missing file", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, err := session.materializeLocalImage(filepath.Join(session.cwd, "gone.png"))
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonMissingFile, data[jsonFieldReason])
	})

	t.Run("directory is not a regular file", func(t *testing.T) {
		session, _ := newImageSession(t)
		dir := filepath.Join(session.cwd, "adir")
		require.NoError(t, os.Mkdir(dir, 0o700))
		_, err := session.materializeLocalImage(dir)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonPathNotAllowed, data[jsonFieldReason])
	})

	t.Run("symlink escaping the allowed roots", func(t *testing.T) {
		session, _ := newImageSession(t)
		outside := filepath.Join(narrowedOutsideRoot(t, session), "outside.png")
		require.NoError(t, os.WriteFile(outside, png, 0o600))
		link := filepath.Join(session.cwd, "escape.png")
		require.NoError(t, os.Symlink(outside, link))

		_, err := session.materializeLocalImage(link)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonPathNotAllowed, data[jsonFieldReason])
	})
}

func TestMaterializeLocalImageSeamFaults(t *testing.T) {
	png := fixtureImage(t, "valid.png")

	writeFixture := func(t *testing.T, sess *session) string {
		t.Helper()

		path := filepath.Join(sess.cwd, "shot.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))

		return path
	}

	t.Run("stat failure", func(t *testing.T) {
		sess, _ := newImageSession(t)
		path := writeFixture(t, sess)
		original := imageStat
		imageStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat boom") }
		t.Cleanup(func() { imageStat = original })
		_, err := sess.materializeLocalImage(path)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonMissingFile, data[jsonFieldReason])
	})

	t.Run("open failure", func(t *testing.T) {
		sess, _ := newImageSession(t)
		path := writeFixture(t, sess)
		original := imageOpen
		imageOpen = func(string) (*os.File, error) { return nil, errors.New("open boom") }
		t.Cleanup(func() { imageOpen = original })
		_, err := sess.materializeLocalImage(path)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonMissingFile, data[jsonFieldReason])
	})

	t.Run("read exceeds limit after stat", func(t *testing.T) {
		sess, _ := newImageSession(t)
		sess.agent.options.ImageLimits = ImageLimits{MaxOutputBytesPerImage: int64(len(png)) + 100}
		path := writeFixture(t, sess)
		original := imageReadAll
		imageReadAll = func(io.Reader) ([]byte, error) { return make([]byte, len(png)+200), nil }
		t.Cleanup(func() { imageReadAll = original })
		_, err := sess.materializeLocalImage(path)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonTooLarge, data[jsonFieldReason])
	})

	t.Run("unresolvable root is skipped", func(t *testing.T) {
		sess, _ := newImageSession(t)
		realDir := t.TempDir()
		path := filepath.Join(realDir, "shot.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))
		// cwd holds no match; the first additional root cannot resolve and is
		// skipped, and the second additional root is the one that allows it.
		sess.cwd = t.TempDir()
		WithScratchDir(t.TempDir())(&sess.agent.options)
		sess.additionalDirectories = []string{filepath.Join(t.TempDir(), "does-not-exist"), realDir}
		decoded, err := sess.materializeLocalImage(path)
		require.NoError(t, err)
		require.Equal(t, png, decoded)
	})
}

func TestToolContentSnapshotReplaceSemantics(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")
	part := opencode.NativePart{CallID: "call-1"}

	t.Run("dedupes across repeated updates", func(t *testing.T) {
		session, _ := newImageSession(t)
		attachments := []opencode.NativeAttachment{{ID: "a", Mime: mimePNG, URL: dataURL(mimePNG, png)}}

		merged, changed, err := session.toolContentSnapshot(ctx, "call-1", part, attachments, false)
		require.NoError(t, err)
		require.True(t, changed)
		require.Len(t, merged, 1)
		session.emittedToolContent["call-1"] = merged

		merged2, changed2, err := session.toolContentSnapshot(ctx, "call-1", part, attachments, false)
		require.NoError(t, err)
		require.False(t, changed2)
		require.Len(t, merged2, 1)
	})

	t.Run("unmapped attachment is skipped", func(t *testing.T) {
		session, _ := newImageSession(t)
		merged, changed, err := session.toolContentSnapshot(ctx, "call-1", part, []opencode.NativeAttachment{
			{ID: "a", Mime: "text/plain"},
		}, false)
		require.NoError(t, err)
		require.False(t, changed)
		require.Empty(t, merged)
	})

	t.Run("remote link rides the array without counting bytes", func(t *testing.T) {
		session, _ := newImageSession(t)
		merged, changed, err := session.toolContentSnapshot(ctx, "call-1", part, []opencode.NativeAttachment{
			{ID: "a", Mime: mimePNG, URL: "https://x/g.png"},
		}, false)
		require.NoError(t, err)
		require.True(t, changed)
		require.Len(t, merged, 1)
		require.False(t, merged[0].isImage)
	})

	t.Run("aggregate too large with distinct images", func(t *testing.T) {
		session, _ := newImageSession(t)
		gif := fixtureImage(t, "valid.gif")
		session.agent.options.ImageLimits = ImageLimits{MaxOutputBytesPerToolCall: int64(len(png)) + 1}
		_, _, err := session.toolContentSnapshot(ctx, "call-1", part, []opencode.NativeAttachment{
			{ID: "a", Mime: mimePNG, URL: dataURL(mimePNG, png)},
			{ID: "b", Mime: mimeGIF, URL: dataURL(mimeGIF, gif)},
		}, false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonTooLarge, data[jsonFieldReason])
		require.Equal(t, int64(len(png)+len(gif)), data[jsonFieldSizeBytes])
	})

	t.Run("mapping failure propagates", func(t *testing.T) {
		session, _ := newImageSession(t)
		_, _, err := session.toolContentSnapshot(ctx, "call-1", part, []opencode.NativeAttachment{
			{ID: "a", Mime: mimePNG},
		}, false)
		require.Error(t, err)
	})
}

func TestMaterializeLocalImageSizeBranches(t *testing.T) {
	png := fixtureImage(t, "valid.png")

	t.Run("stat reports oversize before read", func(t *testing.T) {
		sess, _ := newImageSession(t)
		sess.agent.options.ImageLimits = ImageLimits{MaxOutputBytesPerImage: 1}
		path := filepath.Join(sess.cwd, "big.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))
		_, err := sess.materializeLocalImage(path)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonTooLarge, data[jsonFieldReason])
	})

	t.Run("read failure surfaces missing file", func(t *testing.T) {
		sess, _ := newImageSession(t)
		path := filepath.Join(sess.cwd, "shot.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))
		original := imageReadAll
		imageReadAll = func(io.Reader) ([]byte, error) { return nil, errors.New("read boom") }
		t.Cleanup(func() { imageReadAll = original })
		_, err := sess.materializeLocalImage(path)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonMissingFile, data[jsonFieldReason])
	})
}

func TestMapLocalImageArtifactReplayFinishError(t *testing.T) {
	sess, _ := newImageSession(t)
	// A stored record whose bytes decode cleanly but are not a raster fails at
	// the finish step during replay rather than at record integrity.
	notRaster := base64.StdEncoding.EncodeToString([]byte("plain text, not an image"))
	sess.setImageArtifacts(map[string]imageArtifactRecord{
		"fp": {Version: imageArtifactRecordVersion, NativeID: "id-x", Fingerprint: "fp", Mime: mimePNG, Data: notRaster},
	})
	_, _, err := sess.mapOutputArtifact(context.Background(), opencode.NativeAttachment{Mime: mimePNG, URL: "file:///tmp/x.png"}, "id-x", provenanceTool, true)
	data := assertTurnFailed(t, err, causeTransport, "")
	require.Equal(t, outputReasonNotARaster, data[jsonFieldReason])
}

// TestPathWithinRootRejectsIncomparablePaths covers the containment
// predicate's relation failure: an absolute path has no relation to a
// relative root, which is a containment failure rather than an error.
func TestPathWithinRootRejectsIncomparablePaths(t *testing.T) {
	require.False(t, pathWithinRoot("relative-root", filepath.Join(string(filepath.Separator), "absolute", "path")))
}
