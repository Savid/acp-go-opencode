package opencodeacp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func handoffEnvelope(decoded []byte) map[string]any {
	sum := sha256.Sum256(decoded)

	return map[string]any{
		handoffFieldVersion:   handoffEnvelopeVersion,
		handoffFieldDigest:    hex.EncodeToString(sum[:]),
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

// TestHandoffAndEmbeddedFormsBuildIdenticalNativeRequests pins the two
// transports as interchangeable and the handoff path as absent from
// everything the harness ever sees.
func TestHandoffAndEmbeddedFormsBuildIdenticalNativeRequests(t *testing.T) {
	root := t.TempDir()
	decoded := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "handoff-fixture.png", decoded)

	session := handoffSession(t, root)
	handoffBlocks := []acp.ContentBlock{handoffBlock(mimePNG, path, handoffEnvelope(decoded))}
	embeddedBlocks := []acp.ContentBlock{{Image: &acp.ContentBlockImage{
		Type: "image", MimeType: mimePNG, Data: base64.StdEncoding.EncodeToString(decoded),
	}}}

	resolved, err := session.validatePromptMedia(context.Background(), handoffBlocks)
	require.NoError(t, err)

	viaHandoff, err := promptToOpenCodeParts(handoffBlocks, resolved)
	require.NoError(t, err)

	viaEmbedded, err := promptToOpenCodeParts(embeddedBlocks, nil)
	require.NoError(t, err)
	require.Equal(t, viaEmbedded, viaHandoff)

	commandViaHandoff, err := commandPromptParts(handoffBlocks, resolved)
	require.NoError(t, err)

	commandViaEmbedded, err := commandPromptParts(embeddedBlocks, nil)
	require.NoError(t, err)
	require.Equal(t, commandViaEmbedded, commandViaHandoff)

	for _, parts := range [][]map[string]any{viaHandoff, commandViaHandoff} {
		encoded, err := json.Marshal(parts)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), root)
		require.NotContains(t, string(encoded), "handoff-fixture")
		require.NotContains(t, string(encoded), "file://")
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
				envelope[handoffFieldDigest] = strings.ToUpper(handoffDigest(decoded))
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

	t.Run("escaping symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := writeHandoffFile(t, t.TempDir(), "shot.png", decoded)
		link := filepath.Join(root, "linked.png")
		require.NoError(t, os.Symlink(outside, link))

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, link, envelope),
			imageErrorPathNotAllowed, handoffCauseEscapesRoot)
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

	t.Run("unreadable after resolution", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		restore := imageStat
		imageStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat failed") }
		t.Cleanup(func() { imageStat = restore })

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, envelope),
			imageErrorMissingFile, handoffCauseUninspectable)
	})

	t.Run("cannot be opened", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		restore := imageOpen
		imageOpen = func(string) (*os.File, error) { return nil, errors.New("open failed") }
		t.Cleanup(func() { imageOpen = restore })

		session := handoffSession(t, root)
		requireHandoffVerdict(t, session, handoffBlock(mimePNG, path, envelope),
			imageErrorMissingFile, handoffCauseUnopenable)
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
			imageErrorDigestMismatch, handoffCauseSizeMismatch)
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

	t.Run("a gate below the header still rejects rather than forwarding", func(t *testing.T) {
		// A bound this small truncates the raster header, so the structural
		// chain verdicts before the byte gate reaches its turn. Either way the
		// unverified bytes are rejected, which is the guarantee that matters.
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		session := handoffSession(t, root, WithImageLimits(ImageLimits{MaxInputBytesPerImage: 8}))
		requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock(mimePNG, path, handoffEnvelope(decoded))), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorInvalidDimensions, jsonFieldIndex: 0,
		})
	})

	t.Run("a disabled per image gate still admits a conforming file", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		session := handoffSession(t, root, WithImageLimits(ImageLimits{}))
		require.NoError(t, validatePromptMediaError(session, handoffBlock(mimePNG, path, handoffEnvelope(decoded))))
	})

	t.Run("a disabled per image gate clamps the read to the frame bound", func(t *testing.T) {
		// A disabled byte policy must not mean an unbounded local read, so the
		// decoded frame clamp remains the bound that is at fault.
		root := t.TempDir()
		path := writeHandoffFile(t, root, "huge.png", nil)
		require.NoError(t, os.Truncate(path, imageFrameBoundBytes+1))

		session := handoffSession(t, root, WithImageLimits(ImageLimits{}))
		requireInvalidParamsData(t, validatePromptMediaError(session, handoffBlock(mimePNG, path, handoffEnvelope(decoded))), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
			jsonFieldSizeBytes: imageFrameBoundBytes + 1, jsonFieldMaxBytes: imageFrameBoundBytes,
		})
	})
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

		resolved, err := session.validatePromptMedia(context.Background(), []acp.ContentBlock{block})
		require.NoError(t, err)
		require.Empty(t, resolved)

		parts, err := promptToOpenCodeParts([]acp.ContentBlock{block}, resolved)
		require.NoError(t, err)
		require.Equal(t, "shot.png", parts[0]["filename"])
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
