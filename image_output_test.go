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

	t.Run("path outside allowed roots", func(t *testing.T) {
		session, _ := newImageSession(t)
		// A set scratch dir removes the system temp dir from the allowed roots,
		// so a file in a distinct temp subtree is genuinely outside them.
		session.agent.options.ScratchDir = t.TempDir()
		outside := filepath.Join(t.TempDir(), "elsewhere.png")
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
		// A set scratch dir removes the system temp dir from the allowed roots,
		// so the symlink target below lives in a genuinely outside subtree.
		session.agent.options.ScratchDir = t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.png")
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
		sess.agent.options.ScratchDir = t.TempDir()
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
