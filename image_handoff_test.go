package opencodeacp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// handoffSession returns a session whose agent reads prompt images from root.
func handoffSession(t *testing.T, root string, opts ...Option) *session {
	t.Helper()

	return testSession(NewAgent(append([]Option{WithInputHandoffRoot(root)}, opts...)...), newFakeOpenCodeClient())
}

// writeHandoffFile places bytes under root and returns the absolute path.
func writeHandoffFile(t *testing.T, root, name string, decoded []byte) string {
	t.Helper()

	path := filepath.Join(root, name)
	require.NoError(t, os.WriteFile(path, decoded, 0o600))

	return path
}

func handoffDigestHex(decoded []byte) string {
	sum := sha256.Sum256(decoded)

	return hex.EncodeToString(sum[:])
}

func handoffEnvelope(decoded []byte) map[string]any {
	return map[string]any{
		handoffFieldVersion:   handoffEnvelopeVersion,
		handoffFieldDigest:    handoffDigestHex(decoded),
		handoffFieldSizeBytes: len(decoded),
	}
}

// handoffBlock builds the handoff input form: an image block with empty data,
// a file URI, and a handoff envelope.
func handoffBlock(mime, path string, envelope any) acp.ContentBlock {
	uri := "file://" + filepath.ToSlash(path)
	block := acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mime, Uri: &uri}}

	if envelope != nil {
		block.Image.Meta = map[string]any{handoffEnvelopeKey: envelope}
	}

	return block
}

func TestHandoffFormAcceptsAValidatedFile(t *testing.T) {
	root := t.TempDir()
	decoded := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "shot.png", decoded)

	session := handoffSession(t, root)
	block := handoffBlock(mimePNG, path, handoffEnvelope(decoded))

	resolved, err := session.validatePromptMedia(context.Background(), []acp.ContentBlock{block})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	require.Equal(t, mimePNG, resolved[0].mime)
	require.Equal(t, base64.StdEncoding.EncodeToString(decoded), resolved[0].data)
}

// wrapBase64 breaks an encoded payload into lines the way a host that pretty
// prints its JSON does. The decoder ignores the line breaks, so the bytes are
// the same image while the string is not.
func wrapBase64(payload string, width int) string {
	var lines []string
	for len(payload) > width {
		lines = append(lines, payload[:width])
		payload = payload[width:]
	}

	return strings.Join(append(lines, payload), "\n")
}

// TestHandoffAndEmbeddedFormsBuildIdenticalNativeRequests pins the two
// transports as interchangeable and the handoff path as absent from everything
// the harness ever sees. The embedded fixture carries the two things that once
// made the forms differ: a block URI, and a base64 spelling of its own choosing.
func TestHandoffAndEmbeddedFormsBuildIdenticalNativeRequests(t *testing.T) {
	root := t.TempDir()
	decoded := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "handoff-fixture.png", decoded)

	session := handoffSession(t, root)
	handoffBlocks := []acp.ContentBlock{handoffBlock(mimePNG, path, handoffEnvelope(decoded))}

	embeddedURI := "file:///elsewhere/embedded-fixture.png"
	wrapped := wrapBase64(base64.StdEncoding.EncodeToString(decoded), 76)
	require.Contains(t, wrapped, "\n")
	embeddedBlocks := []acp.ContentBlock{{Image: &acp.ContentBlockImage{
		Type: "image", MimeType: mimePNG, Data: wrapped, Uri: &embeddedURI,
	}}}

	resolvedHandoff, err := session.validatePromptMedia(context.Background(), handoffBlocks)
	require.NoError(t, err)

	resolvedEmbedded, err := session.validatePromptMedia(context.Background(), embeddedBlocks)
	require.NoError(t, err)

	viaHandoff, err := promptToOpenCodeParts(handoffBlocks, resolvedHandoff)
	require.NoError(t, err)

	viaEmbedded, err := promptToOpenCodeParts(embeddedBlocks, resolvedEmbedded)
	require.NoError(t, err)
	require.Equal(t, viaEmbedded, viaHandoff)

	commandViaHandoff, err := commandPromptParts(handoffBlocks, resolvedHandoff)
	require.NoError(t, err)

	commandViaEmbedded, err := commandPromptParts(embeddedBlocks, resolvedEmbedded)
	require.NoError(t, err)
	require.Equal(t, commandViaEmbedded, commandViaHandoff)

	for _, parts := range [][]map[string]any{viaHandoff, commandViaHandoff, viaEmbedded, commandViaEmbedded} {
		encoded, err := json.Marshal(parts)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), root)
		require.NotContains(t, string(encoded), "handoff-fixture")
		require.NotContains(t, string(encoded), "embedded-fixture")
		require.NotContains(t, string(encoded), "file://")
		require.NotContains(t, string(encoded), `\n`)
	}
}

func TestHandoffFormRejectsMalformedBlocks(t *testing.T) {
	root := t.TempDir()
	decoded := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "shot.png", decoded)

	envelopeWith := func(mutate func(map[string]any)) map[string]any {
		envelope := handoffEnvelope(decoded)
		mutate(envelope)

		return envelope
	}

	tests := []struct {
		name    string
		block   acp.ContentBlock
		message string
	}{
		{
			name:    "envelope absent",
			block:   handoffBlock(mimePNG, path, nil),
			message: handoffCauseEnvelopeAbsent,
		},
		{
			name:    "envelope is not an object",
			block:   handoffBlock(mimePNG, path, "not-an-object"),
			message: handoffCauseEnvelopeShape,
		},
		{
			name:    "envelope is null",
			block:   handoffBlock(mimePNG, path, json.RawMessage("null")),
			message: handoffCauseEnvelopeShape,
		},
		{
			name:    "envelope cannot be encoded",
			block:   handoffBlock(mimePNG, path, map[string]any{handoffFieldVersion: func() {}}),
			message: handoffCauseEnvelopeShape,
		},
		{
			name: "envelope carries an unknown field",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope["extra"] = true
			})),
			message: handoffCauseUnknownField,
		},
		{
			name: "version absent",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				delete(envelope, handoffFieldVersion)
			})),
			message: handoffCauseVersion,
		},
		{
			name: "version unsupported",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope[handoffFieldVersion] = 2
			})),
			message: handoffCauseVersion,
		},
		{
			name: "version is a string",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope[handoffFieldVersion] = "1"
			})),
			message: handoffCauseVersion,
		},
		{
			name: "version is fractional",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope[handoffFieldVersion] = 1.5
			})),
			message: handoffCauseVersion,
		},
		{
			name: "digest absent",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				delete(envelope, handoffFieldDigest)
			})),
			message: handoffCauseDigest,
		},
		{
			name: "digest is not a string",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope[handoffFieldDigest] = 12
			})),
			message: handoffCauseDigest,
		},
		{
			name: "digest is too short",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope[handoffFieldDigest] = "abc"
			})),
			message: handoffCauseDigest,
		},
		{
			name: "digest is uppercase",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope[handoffFieldDigest] = strings.ToUpper(handoffDigestHex(decoded))
			})),
			message: handoffCauseDigest,
		},
		{
			name: "sizeBytes absent",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				delete(envelope, handoffFieldSizeBytes)
			})),
			message: handoffCauseSizeBytes,
		},
		{
			name: "sizeBytes is fractional",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope[handoffFieldSizeBytes] = 1.5
			})),
			message: handoffCauseSizeBytes,
		},
		{
			name: "sizeBytes is negative",
			block: handoffBlock(mimePNG, path, envelopeWith(func(envelope map[string]any) {
				envelope[handoffFieldSizeBytes] = -1
			})),
			message: handoffCauseSizeBytes,
		},
		{
			name:    "uri absent",
			block:   acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG, Meta: map[string]any{handoffEnvelopeKey: handoffEnvelope(decoded)}}},
			message: handoffCauseURI,
		},
		{
			name:    "uri empty",
			block:   handoffBlock(mimePNG, "", handoffEnvelope(decoded)),
			message: handoffCauseURI,
		},
		{
			name:    "uri unparseable",
			block:   imageBlockWithURI(mimePNG, "file://%zz", handoffEnvelope(decoded)),
			message: handoffCauseURI,
		},
		{
			name:    "uri is not a file scheme",
			block:   imageBlockWithURI(mimePNG, "https://example.com/shot.png", handoffEnvelope(decoded)),
			message: handoffCauseURI,
		},
		{
			name:    "uri names a foreign host",
			block:   imageBlockWithURI(mimePNG, "file://elsewhere/shot.png", handoffEnvelope(decoded)),
			message: handoffCauseURI,
		},
		{
			name:    "uri path is relative",
			block:   imageBlockWithURI(mimePNG, "file:shot.png", handoffEnvelope(decoded)),
			message: handoffCauseURI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := handoffSession(t, root)
			requireInvalidParamsData(t, validatePromptMediaError(session, tt.block), map[string]any{
				jsonFieldField:   fieldPromptImage,
				jsonFieldError:   imageErrorInvalidHandoff,
				jsonFieldIndex:   0,
				jsonFieldMessage: tt.message,
			})
		})
	}
}

// imageBlockWithURI builds a handoff-form block with a verbatim URI so URI
// defects can be exercised without path joining.
func imageBlockWithURI(mime, uri string, envelope any) acp.ContentBlock {
	block := acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mime, Uri: &uri}}
	block.Image.Meta = map[string]any{handoffEnvelopeKey: envelope}

	return block
}

func TestHandoffFormRejectsAnUnsetRoot(t *testing.T) {
	root := t.TempDir()
	decoded := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "shot.png", decoded)

	session := testSession(NewAgent(), newFakeOpenCodeClient())
	requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock(mimePNG, path, handoffEnvelope(decoded))), map[string]any{
		jsonFieldField:   fieldPromptImage,
		jsonFieldError:   imageErrorInvalidHandoff,
		jsonFieldIndex:   0,
		jsonFieldMessage: handoffCauseRootUnset,
	})
}

func TestHandoffFormRejectsUnreachablePaths(t *testing.T) {
	decoded := fixtureImage(t, "valid.png")
	envelope := handoffEnvelope(decoded)

	t.Run("outside the root", func(t *testing.T) {
		root := t.TempDir()
		outside := writeHandoffFile(t, t.TempDir(), "shot.png", decoded)

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, outside, envelope),
			imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("traversal out of the root", func(t *testing.T) {
		root := t.TempDir()
		outside := writeHandoffFile(t, t.TempDir(), "shot.png", decoded)

		session := handoffSession(t, root)
		block := handoffBlock(mimePNG, filepath.Join(root, "..", filepath.Base(filepath.Dir(outside)), "shot.png"), envelope)
		requireHandoffVerdict(t, session, block, imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("percent-encoded traversal in the uri", func(t *testing.T) {
		root := t.TempDir()

		session := handoffSession(t, root)
		uri := "file://" + filepath.ToSlash(root) + "/%2e%2e/%2e%2e/etc/passwd"
		requireHandoffVerdict(t, session, imageBlockWithURI(mimePNG, uri, envelope),
			imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("a windows drive spelling names nothing inside the root", func(t *testing.T) {
		root := t.TempDir()

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, imageBlockWithURI(mimePNG, "file:///C:/secret.png", envelope),
			imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("symlinks", func(t *testing.T) {
		tests := []struct {
			name    string
			link    func(t *testing.T, root, target string) string
			errCode string
			message string
		}{
			{
				name: "absolute target inside the root",
				link: func(t *testing.T, root, target string) string {
					t.Helper()

					link := filepath.Join(root, "linked.png")
					require.NoError(t, os.Symlink(target, link))

					return link
				},
				errCode: imageErrorPathNotAllowed,
				message: handoffCauseOutsideRoot,
			},
			{
				name: "absolute target outside the root",
				link: func(t *testing.T, root, _ string) string {
					t.Helper()

					outside := writeHandoffFile(t, t.TempDir(), "shot.png", decoded)
					link := filepath.Join(root, "linked.png")
					require.NoError(t, os.Symlink(outside, link))

					return link
				},
				errCode: imageErrorPathNotAllowed,
				message: handoffCauseOutsideRoot,
			},
			{
				name: "relative target outside the root",
				link: func(t *testing.T, root, _ string) string {
					t.Helper()

					outside := writeHandoffFile(t, filepath.Dir(root), "outside.png", decoded)
					link := filepath.Join(root, "linked.png")
					require.NoError(t, os.Symlink(filepath.Join("..", filepath.Base(outside)), link))

					return link
				},
				errCode: imageErrorPathNotAllowed,
				message: handoffCauseOutsideRoot,
			},
			{
				name: "dangling relative target",
				link: func(t *testing.T, root, _ string) string {
					t.Helper()

					link := filepath.Join(root, "linked.png")
					require.NoError(t, os.Symlink("gone.png", link))

					return link
				},
				errCode: imageErrorMissingFile,
				message: handoffCauseAbsent,
			},
			{
				name: "dangling absolute target",
				link: func(t *testing.T, root, _ string) string {
					t.Helper()

					link := filepath.Join(root, "linked.png")
					require.NoError(t, os.Symlink(filepath.Join(root, "gone.png"), link))

					return link
				},
				errCode: imageErrorPathNotAllowed,
				message: handoffCauseOutsideRoot,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				root := t.TempDir()
				target := writeHandoffFile(t, root, "shot.png", decoded)

				session := handoffSession(t, root)
				requireHandoffVerdict(t, session, handoffBlock(mimePNG, tt.link(t, root, target), envelope),
					tt.errCode, tt.message)
			})
		}
	})

	t.Run("not a regular file", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "nested")
		require.NoError(t, os.Mkdir(nested, 0o700))

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, nested, envelope),
			imageErrorPathNotAllowed, handoffCauseNotRegular)
	})

	t.Run("root does not resolve", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "absent")

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, filepath.Join(root, "shot.png"), envelope),
			imageErrorPathNotAllowed, handoffCauseRootUnresolved)
	})

	t.Run("vanished path inside the root", func(t *testing.T) {
		root := t.TempDir()

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, filepath.Join(root, "gone.png"), envelope),
			imageErrorMissingFile, handoffCauseAbsent)
	})

	t.Run("cannot be read", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		restore := imageReadAll
		imageReadAll = func(io.Reader) ([]byte, error) { return nil, errors.New("read failed") }
		t.Cleanup(func() { imageReadAll = restore })

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, envelope),
			imageErrorMissingFile, handoffCauseUnreadable)
	})
}

func TestHandoffFormVerifiesTheDigestFailClosed(t *testing.T) {
	decoded := fixtureImage(t, "valid.png")

	t.Run("byte tamper", func(t *testing.T) {
		root := t.TempDir()
		tampered := append([]byte(nil), decoded...)
		tampered[len(tampered)-1] ^= 0xff
		path := writeHandoffFile(t, root, "shot.png", tampered)

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, handoffEnvelope(decoded)),
			imageErrorDigestMismatch, handoffCauseDigestMismatch)
	})

	t.Run("declared size disagrees", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		envelope := handoffEnvelope(decoded)
		envelope[handoffFieldSizeBytes] = len(decoded) + 1

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, envelope),
			imageErrorDigestMismatch, handoffCauseDigestMismatch)
	})
}

// TestHandoffFormRunsTheEmbeddedGateChain mirrors the embedded-form taxonomy
// over handoff bytes: the same verdicts, minus the two gates that need base64.
func TestHandoffFormRunsTheEmbeddedGateChain(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		mime    string
		want    string
	}{
		{name: "media type outside allowlist", fixture: "valid.png", mime: mimeBMP, want: imageErrorInvalidMediaType},
		{name: "case-variant media type", fixture: "valid.png", mime: "IMAGE/PNG", want: imageErrorInvalidMediaType},
		{name: "truncated raster has no dimensions", fixture: "truncated.png", mime: mimePNG, want: imageErrorInvalidDimensions},
		{name: "declared png with jpeg bytes", fixture: "mismatch.png", mime: mimePNG, want: imageErrorMediaTypeMismatch},
		{name: "animated gif", fixture: "animated.gif", mime: mimeGIF, want: imageErrorAnimated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			decoded := fixtureImage(t, tt.fixture)
			path := writeHandoffFile(t, root, tt.fixture, decoded)

			session := handoffSession(t, root)
			requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock(tt.mime, path, handoffEnvelope(decoded))), map[string]any{
				jsonFieldField: fieldPromptImage, jsonFieldError: tt.want, jsonFieldIndex: 0,
			})
		})
	}

	t.Run("unrecognized bytes are a media type mismatch", func(t *testing.T) {
		root := t.TempDir()
		decoded := []byte("not an image")
		path := writeHandoffFile(t, root, "shot.png", decoded)

		session := handoffSession(t, root)
		requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock(mimePNG, path, handoffEnvelope(decoded))), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMediaTypeMismatch, jsonFieldIndex: 0,
		})
	})
}

// TestHandoffDeclaredMediaTypeIsJudgedBeforeTheFilesystem pins the declared media
// type ahead of every filesystem verdict. The absent-path case is the decisive
// one: the only way to answer it on the declared type is to never have looked, so
// an implementation that opens first reports missing_file and thereby tells the
// caller whether its guess about the path was right.
func TestHandoffDeclaredMediaTypeIsJudgedBeforeTheFilesystem(t *testing.T) {
	root := t.TempDir()
	decoded := fixtureImage(t, "valid.png")
	present := writeHandoffFile(t, root, "shot.png", decoded)

	restore := handoffOpen
	handoffOpen = func(string, string, int) (io.ReadCloser, error) {
		require.FailNow(t, "the filesystem was consulted for a media type the allowlist refuses")

		return nil, errors.New("unreachable")
	}

	t.Cleanup(func() { handoffOpen = restore })

	session := handoffSession(t, root)

	for _, path := range []string{present, filepath.Join(root, "absent.png")} {
		requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock("application/pdf", path, handoffEnvelope(decoded))), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorInvalidMediaType, jsonFieldIndex: 0,
		})
	}
}

func TestHandoffFormEnforcesTheByteGates(t *testing.T) {
	decoded := fixtureImage(t, "valid.png")

	t.Run("per image reports the real file size", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		gate := int64(len(decoded)) - 1
		session := handoffSession(t, root, WithImageLimits(ImageLimits{MaxInputBytesPerImage: gate}))
		requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock(mimePNG, path, handoffEnvelope(decoded))), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
			jsonFieldSizeBytes: int64(len(decoded)), jsonFieldMaxBytes: gate,
		})
	})

	t.Run("per prompt aggregate counts handoff bytes", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)
		size := int64(len(decoded))

		session := handoffSession(t, root, WithImageLimits(ImageLimits{MaxInputBytesPerPrompt: size + 1}))
		block := handoffBlock(mimePNG, path, handoffEnvelope(decoded))
		requireInvalidParamsData(t, validatePromptMediaError(session, block, block), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 1,
			jsonFieldSizeBytes: 2 * size, jsonFieldMaxBytes: size + 1,
		})
	})

	t.Run("a bound below the declared size rejects before the read", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		reads := 0
		restore := imageReadAll
		imageReadAll = func(reader io.Reader) ([]byte, error) {
			reads++

			return io.ReadAll(reader)
		}
		t.Cleanup(func() { imageReadAll = restore })

		session := handoffSession(t, root, WithImageLimits(ImageLimits{MaxInputBytesPerImage: 8}))
		requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock(mimePNG, path, handoffEnvelope(decoded))), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
			jsonFieldSizeBytes: int64(len(decoded)), jsonFieldMaxBytes: int64(8),
		})
		require.Zero(t, reads, "a block the declared size already fails must not be read")
	})

	// Both clamped configurations are driven with a declaration one byte past the
	// frame bound and a real, valid file behind it, so the clamp is the only thing
	// that can produce the refusal.
	for _, clamped := range []struct {
		name   string
		limits ImageLimits
	}{
		{name: "a disabled per image gate clamps to the frame bound", limits: ImageLimits{}},
		{
			name:   "a configured gate wider than a frame is clamped to the frame bound",
			limits: ImageLimits{MaxInputBytesPerImage: 100 * imageFrameBoundBytes},
		},
	} {
		t.Run(clamped.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeHandoffFile(t, root, "shot.png", decoded)

			declaration := handoffEnvelope(decoded)
			declaration[handoffFieldSizeBytes] = imageFrameBoundBytes + 1

			session := handoffSession(t, root, WithImageLimits(clamped.limits))
			requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock(mimePNG, path, declaration)), map[string]any{
				jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
				jsonFieldSizeBytes: imageFrameBoundBytes + 1, jsonFieldMaxBytes: imageFrameBoundBytes,
			})
		})
	}
}

// TestHandoffFormSelection pins the pre-gate's form selection: embedded data
// always wins, and a block with neither data nor handoff intent stays the
// embedded form's missing data.
func TestHandoffFormSelection(t *testing.T) {
	root := t.TempDir()
	decoded := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "shot.png", decoded)
	payload := base64.StdEncoding.EncodeToString(decoded)

	t.Run("data wins over a handoff envelope", func(t *testing.T) {
		session := handoffSession(t, root)
		block := handoffBlock(mimePNG, path, handoffEnvelope(decoded))
		block.Image.Data = payload

		// The file under the root holds bytes the embedded data does not, so a
		// part built from the file rather than the data would be visible here.
		require.NoError(t, os.WriteFile(path, fixtureImage(t, "valid.gif"), 0o600))

		resolved, err := session.validatePromptMedia(context.Background(), []acp.ContentBlock{block})
		require.NoError(t, err)

		parts, err := promptToOpenCodeParts([]acp.ContentBlock{block}, resolved)
		require.NoError(t, err)
		require.Equal(t, map[string]any{
			jsonFieldType: partTypeFile,
			jsonFieldMime: mimePNG,
			jsonFieldURL:  "data:" + mimePNG + ";base64," + payload,
		}, parts[0])

		require.NoError(t, os.WriteFile(path, decoded, 0o600))
	})

	t.Run("data wins even when the envelope is malformed", func(t *testing.T) {
		session := handoffSession(t, root)
		block := handoffBlock(mimePNG, path, "not-an-object")
		block.Image.Data = payload

		require.NoError(t, validatePromptMediaError(session, block))
	})

	t.Run("no data and no intent is missing data", func(t *testing.T) {
		session := handoffSession(t, root)
		requireInvalidParamsData(t, validatePromptMediaError(session,
			acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG}},
		), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMissingData, jsonFieldIndex: 0,
		})
	})

	t.Run("a remote uri without data is missing data", func(t *testing.T) {
		remote := "https://example.com/shot.png"
		session := handoffSession(t, root)
		requireInvalidParamsData(t, validatePromptMediaError(session,
			acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG, Uri: &remote}},
		), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMissingData, jsonFieldIndex: 0,
		})
	})

	t.Run("an unparseable uri without data is missing data", func(t *testing.T) {
		broken := "file://%zz"
		session := handoffSession(t, root)
		requireInvalidParamsData(t, validatePromptMediaError(session,
			acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG, Uri: &broken}},
		), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMissingData, jsonFieldIndex: 0,
		})
	})

	t.Run("a file uri alone signals handoff intent", func(t *testing.T) {
		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, nil),
			imageErrorInvalidHandoff, handoffCauseEnvelopeAbsent)
	})

	t.Run("a loopback host resolves", func(t *testing.T) {
		session := handoffSession(t, root)
		block := imageBlockWithURI(mimePNG, "file://localhost"+filepath.ToSlash(path), handoffEnvelope(decoded))
		require.NoError(t, validatePromptMediaError(session, block))
	})
}

func TestInputHandoffRootValidation(t *testing.T) {
	require.NoError(t, validateInputHandoffRoot(""))
	require.NoError(t, validateInputHandoffRoot(filepath.Join(string(filepath.Separator), "srv", "handoff")))
	require.Error(t, validateInputHandoffRoot(filepath.Join("relative", "handoff")))

	_, err := NewAgent(WithInputHandoffRoot("relative")).Initialize(context.Background(), acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	})
	require.Error(t, err)

	// An in-process host is free to skip initialize entirely, so the refusal
	// cannot live only there: opening a session under a root this agent already
	// rejected would run a whole turn against options that never validated.
	_, err = NewAgent(WithInputHandoffRoot("relative")).NewSession(context.Background(), acp.NewSessionRequest{
		Cwd: t.TempDir(),
	})
	require.ErrorContains(t, err, "input handoff root must be an absolute path")

	require.Empty(t, (&session{}).inputHandoffRoot())
}

func requireHandoffVerdict(t *testing.T, session *session, block acp.ContentBlock, errValue, message string) {
	t.Helper()

	requireInvalidParamsData(t, validatePromptMediaError(session, block), map[string]any{
		jsonFieldField:   fieldPromptImage,
		jsonFieldError:   errValue,
		jsonFieldIndex:   0,
		jsonFieldMessage: message,
	})
}

// TestHandoffUnderDeclaredFileIsRejectedAndForwardsNoBytes drives the shape a
// process sharing the read root produces: the file named by the block grows
// while it is being read, well inside the byte gate the whole time. The gate
// therefore cannot be what refuses it — only reading to the declaration and
// finding more can — and the bytes that come back are not the bytes the envelope
// describes, so none of them may reach the harness or be charged as if they were.
func TestHandoffUnderDeclaredFileIsRejectedAndForwardsNoBytes(t *testing.T) {
	root := t.TempDir()
	png := fixtureImage(t, "valid.png")
	gate := int64(len(png)) + 512
	path := writeHandoffFile(t, root, "shot.png", png)

	// A run of one byte value, so its base64 is recognisable wherever it lands.
	filler := bytes.Repeat([]byte{0x41}, 256)
	marker := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 48))

	read := 0
	restore := imageReadAll
	imageReadAll = func(reader io.Reader) ([]byte, error) {
		handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		require.NoError(t, err)

		_, err = handle.Write(filler)
		require.NoError(t, err)
		require.NoError(t, handle.Close())

		data, err := io.ReadAll(reader)
		read += len(data)

		return data, err
	}
	t.Cleanup(func() { imageReadAll = restore })

	// The aggregate is exactly what the two declared sizes add up to, so a
	// charge taken from the envelope rather than from the bytes read would let
	// the whole prompt through.
	session := handoffSession(t, root, WithImageLimits(ImageLimits{
		MaxInputBytesPerImage:  gate,
		MaxInputBytesPerPrompt: 2 * int64(len(png)),
	}))

	blocks := []acp.ContentBlock{
		{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG, Data: base64.StdEncoding.EncodeToString(png)}},
		handoffBlock(mimePNG, path, handoffEnvelope(png)),
	}

	resolved, err := session.validatePromptMedia(context.Background(), blocks)
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorDigestMismatch, jsonFieldIndex: 1,
		jsonFieldMessage: handoffCauseDigestMismatch,
	})

	// The read stopped one byte past the declaration, so a file that grew under
	// the adapter cost it the size its host declared and not the size it became.
	require.LessOrEqual(t, read, len(png)+1)

	parts, err := promptToOpenCodeParts(blocks, resolved)
	require.NoError(t, err)

	encoded, err := json.Marshal(parts)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), marker, "bytes past the declaration reached the native request")
}

// TestHandoffBlockCountIsCappedWithTheAggregateDisabled pins the bound on the
// work a prompt can ask for when no byte aggregate is configured. Every block
// is a conforming image well inside the per-image gate, so only the count can
// decide, and the block that crosses is refused before it is read.
func TestHandoffBlockCountIsCappedWithTheAggregateDisabled(t *testing.T) {
	root := t.TempDir()
	png := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "shot.png", png)

	reads := 0
	restore := imageReadAll
	imageReadAll = func(reader io.Reader) ([]byte, error) {
		reads++

		return io.ReadAll(reader)
	}
	t.Cleanup(func() { imageReadAll = restore })

	blocks := make([]acp.ContentBlock, 0, maxHandoffBlocksPerPrompt+1)
	for range maxHandoffBlocksPerPrompt + 1 {
		blocks = append(blocks, handoffBlock(mimePNG, path, handoffEnvelope(png)))
	}

	session := handoffSession(t, root, WithImageLimits(ImageLimits{MaxInputBytesPerImage: defaultImageLimitBytes}))

	resolved, err := session.validatePromptMedia(context.Background(), blocks[:maxHandoffBlocksPerPrompt])
	require.NoError(t, err)
	require.Len(t, resolved, maxHandoffBlocksPerPrompt)
	require.Equal(t, maxHandoffBlocksPerPrompt, reads)

	reads = 0
	requireInvalidParamsData(t, validatePromptMediaError(session, blocks...), map[string]any{
		jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge,
		jsonFieldIndex:     maxHandoffBlocksPerPrompt,
		jsonFieldSizeBytes: int64(maxHandoffBlocksPerPrompt + 1),
		jsonFieldMaxBytes:  int64(maxHandoffBlocksPerPrompt),
	})
	require.Equal(t, maxHandoffBlocksPerPrompt, reads, "the block that crossed the cap was read")
}

// TestHandoffUnsetRootAnswersAheadOfTheBlockCap pins the order of the two
// pre-gate refusals one prompt can earn at once. An adapter with no read root
// has no handoff work for the cap to bound, and invalid_handoff is what tells a
// host its read root never reached the agent, so a prompt carrying more blocks
// than the cap must still be answered with the root and not with too_large.
func TestHandoffUnsetRootAnswersAheadOfTheBlockCap(t *testing.T) {
	root := t.TempDir()
	png := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "shot.png", png)

	blocks := make([]acp.ContentBlock, 0, maxHandoffBlocksPerPrompt+1)
	for range maxHandoffBlocksPerPrompt + 1 {
		blocks = append(blocks, handoffBlock(mimePNG, path, handoffEnvelope(png)))
	}

	session := testSession(NewAgent(), newFakeOpenCodeClient())

	requireInvalidParamsData(t, validatePromptMediaError(session, blocks...), map[string]any{
		jsonFieldField:   fieldPromptImage,
		jsonFieldError:   imageErrorInvalidHandoff,
		jsonFieldIndex:   0,
		jsonFieldMessage: handoffCauseRootUnset,
	})
}

// TestHandoffFileSwappedAfterContainmentIsRejected replaces the named file
// between one read of it and the next. Neither substitution reaches the
// harness: a name that now leaves the root is refused by the open, and a
// different file that stays inside it is refused by the digest. What no longer
// exists is a substitution that satisfies both.
func TestHandoffFileSwappedAfterContainmentIsRejected(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	substitute := fixtureImage(t, "valid.gif")

	tests := []struct {
		name    string
		swap    func(t *testing.T, path string)
		errCode string
		message string
	}{
		{
			name: "replaced by a symlink out of the root",
			swap: func(t *testing.T, path string) {
				t.Helper()

				outside := writeHandoffFile(t, t.TempDir(), "target.png", substitute)
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(outside, path))
			},
			errCode: imageErrorPathNotAllowed,
			message: handoffCauseOutsideRoot,
		},
		{
			name: "replaced by a different regular file",
			swap: func(t *testing.T, path string) {
				t.Helper()

				require.NoError(t, os.Remove(path))
				require.NoError(t, os.WriteFile(path, substitute, 0o600))
			},
			errCode: imageErrorDigestMismatch,
			message: handoffCauseDigestMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeHandoffFile(t, root, "shot.png", png)

			swapped := false
			restore := imageReadAll
			imageReadAll = func(reader io.Reader) ([]byte, error) {
				if !swapped {
					swapped = true

					tt.swap(t, path)
				}

				return io.ReadAll(reader)
			}
			t.Cleanup(func() { imageReadAll = restore })

			session := handoffSession(t, root)
			block := handoffBlock(mimePNG, path, handoffEnvelope(png))

			// The first read is served from the descriptor already open on the
			// declared file, so it still yields the declared bytes; the swap
			// lands behind it and the next block meets the substitute.
			resolved, err := session.validatePromptMedia(context.Background(), []acp.ContentBlock{block})
			require.NoError(t, err)
			require.Equal(t, base64.StdEncoding.EncodeToString(png), resolved[0].data)

			requireHandoffVerdict(t, session, block, tt.errCode, tt.message)
		})
	}
}

// TestHandoffMessagesAreConstants drives every handoff verdict across hostile
// inputs and pins what the human message may say: one of the declared causes,
// and never the root, the file name, the digest, or a size the caller did not
// itself declare.
func TestHandoffMessagesAreConstants(t *testing.T) {
	causes := []string{
		handoffCauseRootUnset,
		handoffCauseEnvelopeAbsent,
		handoffCauseEnvelopeShape,
		handoffCauseUnknownField,
		handoffCauseVersion,
		handoffCauseDigest,
		handoffCauseSizeBytes,
		handoffCauseURI,
		handoffCauseRootUnresolved,
		handoffCauseOutsideRoot,
		handoffCauseNotRegular,
		handoffCauseAbsent,
		handoffCauseUnreadable,
		handoffCauseDigestMismatch,
	}

	decoded := fixtureImage(t, "valid.png")
	digest := handoffDigestHex(decoded)

	tests := []struct {
		name    string
		errCode string
		build   func(t *testing.T, root string) (acp.ContentBlock, []Option)
	}{
		{
			name:    "no read root is configured",
			errCode: imageErrorInvalidHandoff,
			build: func(t *testing.T, root string) (acp.ContentBlock, []Option) {
				t.Helper()

				path := writeHandoffFile(t, root, "secret-name.png", decoded)

				return handoffBlock(mimePNG, path, handoffEnvelope(decoded)), nil
			},
		},
		{
			name:    "the envelope is absent",
			errCode: imageErrorInvalidHandoff,
			build: func(t *testing.T, root string) (acp.ContentBlock, []Option) {
				t.Helper()

				path := writeHandoffFile(t, root, "secret-name.png", decoded)

				return handoffBlock(mimePNG, path, nil), []Option{WithInputHandoffRoot(root)}
			},
		},
		{
			name:    "the uri is not a local file",
			errCode: imageErrorInvalidHandoff,
			build: func(t *testing.T, root string) (acp.ContentBlock, []Option) {
				t.Helper()

				return imageBlockWithURI(mimePNG, "https://example.com/secret-name.png", handoffEnvelope(decoded)),
					[]Option{WithInputHandoffRoot(root)}
			},
		},
		{
			name:    "the path escapes the root",
			errCode: imageErrorPathNotAllowed,
			build: func(t *testing.T, root string) (acp.ContentBlock, []Option) {
				t.Helper()

				outside := writeHandoffFile(t, t.TempDir(), "secret-name.png", decoded)

				return handoffBlock(mimePNG, outside, handoffEnvelope(decoded)), []Option{WithInputHandoffRoot(root)}
			},
		},
		{
			name:    "the path is not a regular file",
			errCode: imageErrorPathNotAllowed,
			build: func(t *testing.T, root string) (acp.ContentBlock, []Option) {
				t.Helper()

				nested := filepath.Join(root, "secret-name.png")
				require.NoError(t, os.Mkdir(nested, 0o700))

				return handoffBlock(mimePNG, nested, handoffEnvelope(decoded)), []Option{WithInputHandoffRoot(root)}
			},
		},
		{
			name:    "the file is gone",
			errCode: imageErrorMissingFile,
			build: func(t *testing.T, root string) (acp.ContentBlock, []Option) {
				t.Helper()

				return handoffBlock(mimePNG, filepath.Join(root, "secret-name.png"), handoffEnvelope(decoded)),
					[]Option{WithInputHandoffRoot(root)}
			},
		},
		{
			name:    "the bytes were tampered with",
			errCode: imageErrorDigestMismatch,
			build: func(t *testing.T, root string) (acp.ContentBlock, []Option) {
				t.Helper()

				tampered := append([]byte(nil), decoded...)
				tampered[len(tampered)-1] ^= 0xff
				path := writeHandoffFile(t, root, "secret-name.png", tampered)

				return handoffBlock(mimePNG, path, handoffEnvelope(decoded)), []Option{WithInputHandoffRoot(root)}
			},
		},
		{
			name:    "the declared size disagrees",
			errCode: imageErrorDigestMismatch,
			build: func(t *testing.T, root string) (acp.ContentBlock, []Option) {
				t.Helper()

				path := writeHandoffFile(t, root, "secret-name.png", decoded[:len(decoded)-1])

				return handoffBlock(mimePNG, path, handoffEnvelope(decoded)), []Option{WithInputHandoffRoot(root)}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			block, options := tt.build(t, root)

			session := testSession(NewAgent(options...), newFakeOpenCodeClient())

			var reqErr *acp.RequestError
			require.ErrorAs(t, validatePromptMediaError(session, block), &reqErr)

			data, ok := reqErr.Data.(map[string]any)
			require.True(t, ok)
			require.Equal(t, tt.errCode, data[jsonFieldError])

			message, ok := data[jsonFieldMessage].(string)
			require.True(t, ok)
			require.Contains(t, causes, message)
			require.NotContains(t, message, root)
			require.NotContains(t, message, "secret-name")
			require.NotContains(t, message, digest)
			require.NotContains(t, message, strconv.Itoa(len(decoded)))
			require.NotContains(t, message, string(filepath.Separator))
		})
	}
}

// TestHandoffReadHonoursContextCancellation pins the validation walk and the
// read as cancellable: a turn that has already been abandoned releases its slot
// instead of spending reads on it.
func TestHandoffReadHonoursContextCancellation(t *testing.T) {
	root := t.TempDir()
	png := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "shot.png", png)
	block := handoffBlock(mimePNG, path, handoffEnvelope(png))

	countedReads := func(t *testing.T) *int {
		t.Helper()

		reads := 0
		restore := imageReadAll
		imageReadAll = func(reader io.Reader) ([]byte, error) {
			reads++

			return io.ReadAll(reader)
		}
		t.Cleanup(func() { imageReadAll = restore })

		return &reads
	}

	t.Run("before the prompt is walked", func(t *testing.T) {
		reads := countedReads(t)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		session := handoffSession(t, root)

		_, err := session.validatePromptMedia(ctx, []acp.ContentBlock{block})
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, *reads)
	})

	t.Run("before the file is opened", func(t *testing.T) {
		reads := countedReads(t)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		session := handoffSession(t, root)

		media := promptMediaBlocks([]acp.ContentBlock{block})
		_, err := session.readHandoffImage(ctx, media[0], defaultImageLimits())
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, *reads)
	})
}

// TestHandoffEnvelopeNumericBounds pins the numeric envelope fields against the
// values that sit on a float's edges, and against a decoder that hands them
// over as json.Number rather than as float64.
func TestHandoffEnvelopeNumericBounds(t *testing.T) {
	root := t.TempDir()
	decoded := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "shot.png", decoded)

	envelopeWith := func(field string, value any) map[string]any {
		envelope := handoffEnvelope(decoded)
		envelope[field] = value

		return envelope
	}

	t.Run("version zero is unsupported", func(t *testing.T) {
		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, envelopeWith(handoffFieldVersion, 0)),
			imageErrorInvalidHandoff, handoffCauseVersion)
	})

	t.Run("sizeBytes beyond an int64 is rejected as an envelope defect", func(t *testing.T) {
		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, envelopeWith(handoffFieldSizeBytes, math.Pow(2, 63))),
			imageErrorInvalidHandoff, handoffCauseSizeBytes)
	})

	t.Run("sizeBytes at the exact float integer limit is a size verdict", func(t *testing.T) {
		// 2^53 is a well-formed envelope value, so it survives the envelope
		// gate and is answered by the bound instead.
		session := handoffSession(t, root, WithImageLimits(ImageLimits{MaxInputBytesPerImage: defaultImageLimitBytes}))
		requireInvalidParamsData(t, validatePromptMediaError(session,
			handoffBlock(mimePNG, path, envelopeWith(handoffFieldSizeBytes, math.Pow(2, 53))),
		), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
			jsonFieldSizeBytes: int64(1) << 53, jsonFieldMaxBytes: defaultImageLimitBytes,
		})
	})

	t.Run("numbers decoded as json.Number still validate", func(t *testing.T) {
		wire, err := json.Marshal(map[string]any{handoffEnvelopeKey: handoffEnvelope(decoded)})
		require.NoError(t, err)

		decoder := json.NewDecoder(bytes.NewReader(wire))
		decoder.UseNumber()

		var meta map[string]any
		require.NoError(t, decoder.Decode(&meta))

		envelope, ok := meta[handoffEnvelopeKey].(map[string]any)
		require.True(t, ok)
		require.IsType(t, json.Number(""), envelope[handoffFieldVersion])

		uri := "file://" + filepath.ToSlash(path)
		block := acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG, Uri: &uri, Meta: meta}}

		session := handoffSession(t, root)
		require.NoError(t, validatePromptMediaError(session, block))
	})
}

// TestHandoffHardlinkInsideTheRootIsRead records the read root's real reach: it
// binds where a name may resolve, not which bytes a directory entry can point
// at, so a hardlink placed inside it is read like any other file. The digest,
// not the root, is what makes the transport trustworthy.
func TestHandoffHardlinkInsideTheRootIsRead(t *testing.T) {
	root := t.TempDir()
	png := fixtureImage(t, "valid.png")
	outside := writeHandoffFile(t, t.TempDir(), "outside.png", png)
	linked := filepath.Join(root, "linked.png")
	require.NoError(t, os.Link(outside, linked))

	session := handoffSession(t, root)

	resolved, err := session.validatePromptMedia(context.Background(), []acp.ContentBlock{
		handoffBlock(mimePNG, linked, handoffEnvelope(png)),
	})
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString(png), resolved[0].data)
}

// countedCloser reports every release of a descriptor the handoff opener
// handed out.
type countedCloser struct {
	io.ReadCloser

	closed *int
}

func (c countedCloser) Close() error {
	*c.closed++

	return c.ReadCloser.Close()
}

// countHandoffDescriptors makes a leaked descriptor a test failure rather than
// a quiet one. The opener proves containment and the file kind before it
// returns, so every gate that runs afterwards holds an open file and every
// refusal among them is a place a release can be forgotten.
func countHandoffDescriptors(t *testing.T) func() {
	t.Helper()

	var opened, closed int

	restore := handoffOpen
	handoffOpen = func(dir, path string, index int) (io.ReadCloser, error) {
		file, err := restore(dir, path, index)
		if err != nil {
			return nil, err
		}

		opened++

		return countedCloser{ReadCloser: file, closed: &closed}, nil
	}

	t.Cleanup(func() { handoffOpen = restore })

	return func() {
		t.Helper()

		require.Positive(t, opened, "no handoff descriptor was opened")
		require.Equal(t, opened, closed, "a handoff descriptor was left open")
	}
}

// TestHandoffRefusalsReleaseTheirDescriptor walks every verdict that is decided
// while a handoff file is open and pins each one as releasing it.
func TestHandoffRefusalsReleaseTheirDescriptor(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	gate := int64(len(png)) + 512

	// The verdicts a declaration decides on its own are not here: they are settled
	// before anything is opened, and this test requires a descriptor to exist.
	tests := []struct {
		name    string
		mime    string
		grow    bool
		size    any
		errCode string
	}{
		{name: "accepted", mime: mimePNG, errCode: ""},
		{name: "grown past the declaration while read", mime: mimePNG, grow: true, errCode: imageErrorDigestMismatch},
		{name: "bytes disagree with the envelope", mime: mimePNG, size: int64(len(png)) - 1, errCode: imageErrorDigestMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeHandoffFile(t, root, "shot.png", png)

			if tt.grow {
				restore := imageReadAll
				imageReadAll = func(reader io.Reader) ([]byte, error) {
					handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
					require.NoError(t, err)

					_, err = handle.Write(bytes.Repeat([]byte{0x41}, 256))
					require.NoError(t, err)
					require.NoError(t, handle.Close())

					return io.ReadAll(reader)
				}
				t.Cleanup(func() { imageReadAll = restore })
			}

			envelope := handoffEnvelope(png)
			if tt.size != nil {
				envelope[handoffFieldSizeBytes] = tt.size
			}

			requireBalanced := countHandoffDescriptors(t)

			session := handoffSession(t, root, WithImageLimits(ImageLimits{MaxInputBytesPerImage: gate}))
			err := validatePromptMediaError(session, handoffBlock(tt.mime, path, envelope))

			if tt.errCode == "" {
				require.NoError(t, err)
			} else {
				var reqErr *acp.RequestError
				require.ErrorAs(t, err, &reqErr)

				data, ok := reqErr.Data.(map[string]any)
				require.True(t, ok)
				require.Equal(t, tt.errCode, data[jsonFieldError])
			}

			requireBalanced()
		})
	}
}
