package opencodeacp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// validatePromptMediaError runs prompt media validation and keeps only the
// verdict, which is what the taxonomy assertions compare.
func validatePromptMediaError(session *session, blocks ...acp.ContentBlock) error {
	_, err := session.validatePromptMedia(context.Background(), blocks)

	return err
}

func blobResourceBlock(blob string, mime *string) acp.ContentBlock {
	return acp.ContentBlock{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
		BlobResourceContents: &acp.BlobResourceContents{Blob: blob, Uri: "file:///tmp/blob", MimeType: mime},
	}}}
}

func TestValidatePromptImagesInputTaxonomy(t *testing.T) {
	png := fixtureImageBase64(t, "valid.png")

	tests := []struct {
		name  string
		block acp.ContentBlock
		want  map[string]any
	}{
		{
			name:  "missing data",
			block: acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG}},
			want:  map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMissingData, jsonFieldIndex: 0},
		},
		{
			name:  "media type outside allowlist",
			block: acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimeBMP}},
			want:  map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorInvalidMediaType, jsonFieldIndex: 0},
		},
		{
			name:  "blob resource case-variant media type",
			block: blobResourceBlock(png, stringPtr("IMAGE/PNG")),
			want:  map[string]any{jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorInvalidMediaType, jsonFieldIndex: 0},
		},
		{
			name:  "blob resource leading-whitespace media type",
			block: blobResourceBlock(png, stringPtr(" image/png")),
			want:  map[string]any{jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorInvalidMediaType, jsonFieldIndex: 0},
		},
		{
			name:  "blob resource parameterized media type",
			block: blobResourceBlock(png, stringPtr("image/png; charset=binary")),
			want:  map[string]any{jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorInvalidMediaType, jsonFieldIndex: 0},
		},
		{
			name:  "blob resource rejected deep in the raster chain",
			block: blobResourceBlock(fixtureImageBase64(t, "truncated.png"), stringPtr(mimePNG)),
			want:  map[string]any{jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorInvalidDimensions, jsonFieldIndex: 0},
		},
		{
			name:  "recognized raster with bad dimensions and mismatched declared type",
			block: acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: fixtureImageBase64(t, "truncated.png"), MimeType: mimeJPEG}},
			want:  map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorInvalidDimensions, jsonFieldIndex: 0},
		},
		{
			name:  "invalid base64",
			block: acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: "!!!!", MimeType: mimePNG}},
			want:  map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorInvalidBase64, jsonFieldIndex: 0},
		},
		{
			name:  "declared png with jpeg bytes",
			block: acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: fixtureImageBase64(t, "mismatch.png"), MimeType: mimePNG}},
			want:  map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMediaTypeMismatch, jsonFieldIndex: 0},
		},
		{
			name:  "truncated png has no dimensions",
			block: acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: fixtureImageBase64(t, "truncated.png"), MimeType: mimePNG}},
			want:  map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorInvalidDimensions, jsonFieldIndex: 0},
		},
		{
			name:  "unsniffable declared image",
			block: acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("not an image")), MimeType: mimePNG}},
			want:  map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMediaTypeMismatch, jsonFieldIndex: 0},
		},
		{
			name:  "animated gif",
			block: acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: fixtureImageBase64(t, "animated.gif"), MimeType: mimeGIF}},
			want:  map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorAnimated, jsonFieldIndex: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := testSession(NewAgent(), newFakeOpenCodeClient())
			requireInvalidParamsData(t, validatePromptMediaError(session, tt.block), tt.want)
		})
	}
}

func TestValidatePromptImagesSizeLimits(t *testing.T) {
	png := fixtureImageBase64(t, "valid.png")
	decodedSize := int64(len(fixtureImage(t, "valid.png")))

	t.Run("per image", func(t *testing.T) {
		session := testSession(NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerImage: 1})), newFakeOpenCodeClient())
		requireInvalidParamsData(t, validatePromptMediaError(session,
			acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimePNG}},
		), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
			jsonFieldSizeBytes: decodedSize, jsonFieldMaxBytes: int64(1),
		})
	})

	t.Run("per prompt aggregate", func(t *testing.T) {
		session := testSession(NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerPrompt: decodedSize + 1})), newFakeOpenCodeClient())
		err := validatePromptMediaError(session,
			acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimePNG}},
			acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimePNG}},
		)
		requireInvalidParamsData(t, err, map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 1,
			jsonFieldSizeBytes: 2 * decodedSize, jsonFieldMaxBytes: decodedSize + 1,
		})
	})
}

// TestValidatePromptMediaGatesBlobResourceChannel pins the embedded resource
// blob channel closed for a blob of any MIME: base64 validity, the per-image
// decoded-byte gate, and per-prompt accounting all apply, while the blob's
// native representation is left exactly as it was.
func TestValidatePromptMediaGatesBlobResourceChannel(t *testing.T) {
	pdfMime := "application/pdf"

	t.Run("oversize pdf blob is rejected", func(t *testing.T) {
		// Larger than the per-image limit the same adapter enforces for an
		// image blob, which this channel previously accepted unbounded.
		oversize := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("P"), 6295951))
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		requireInvalidParamsData(t, validatePromptMediaError(session, blobResourceBlock(oversize, &pdfMime)), map[string]any{
			jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
			jsonFieldSizeBytes: int64(6295951), jsonFieldMaxBytes: defaultImageLimitBytes,
		})
	})

	t.Run("corrupt base64 blob is rejected", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		requireInvalidParamsData(t, validatePromptMediaError(session, blobResourceBlock("!!!!", &pdfMime)), map[string]any{
			jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorInvalidBase64, jsonFieldIndex: 0,
		})
	})

	t.Run("blob bytes count toward per-prompt accounting", func(t *testing.T) {
		png := fixtureImage(t, "valid.png")
		document := bytes.Repeat([]byte("P"), 64)
		limit := int64(len(png)) + int64(len(document)) - 1

		session := testSession(NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerPrompt: limit})), newFakeOpenCodeClient())
		requireInvalidParamsData(t, validatePromptMediaError(session,
			blobResourceBlock(base64.StdEncoding.EncodeToString(document), &pdfMime),
			acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: base64.StdEncoding.EncodeToString(png), MimeType: mimePNG}},
		), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 1,
			jsonFieldSizeBytes: int64(len(document)) + int64(len(png)), jsonFieldMaxBytes: limit,
		})
	})

	t.Run("conforming pdf blob still maps to its unchanged native form", func(t *testing.T) {
		document := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7"))
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		block := blobResourceBlock(document, &pdfMime)
		require.NoError(t, validatePromptMediaError(session, block))

		parts, err := promptToOpenCodeParts([]acp.ContentBlock{block}, nil)
		require.NoError(t, err)
		require.Equal(t, map[string]any{jsonFieldType: partTypeText, partTypeText: "file:///tmp/blob"}, parts[0])
	})

	t.Run("blob without data is left alone", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		require.NoError(t, validatePromptMediaError(session, blobResourceBlock("", &pdfMime)))
	})

	t.Run("blob without a declared media type is gated as a document", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		requireInvalidParamsData(t, validatePromptMediaError(session, blobResourceBlock("!!!!", nil)), map[string]any{
			jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorInvalidBase64, jsonFieldIndex: 0,
		})
	})

	// The native part a blob resource builds carries the re-encoding of the bytes
	// the gates measured, exactly as an image block's does. Forwarding the host's
	// own spelling would let one payload reach the harness in as many shapes as
	// the decoder tolerates, none of them the shape validation inspected.
	t.Run("blob base64 is re-encoded from the bytes the gates measured", func(t *testing.T) {
		pngMime := mimePNG
		png := fixtureImage(t, "valid.png")
		canonical := base64.StdEncoding.EncodeToString(png)
		wrapped := wrapBase64(canonical, 76)
		require.Contains(t, wrapped, "\n")

		session := testSession(NewAgent(), newFakeOpenCodeClient())

		for _, mime := range []*string{&pngMime, &pdfMime} {
			blocks := []acp.ContentBlock{blobResourceBlock(wrapped, mime)}

			resolved, err := session.validatePromptMedia(context.Background(), blocks)
			require.NoError(t, err)
			require.Equal(t, canonical, resolved[0].data)

			// The command path inlines every blob whatever its media type, so it
			// is where a verbatim forward would still reach the harness.
			parts, err := commandPromptParts(blocks, resolved)
			require.NoError(t, err)

			encoded, err := json.Marshal(parts)
			require.NoError(t, err)
			require.Contains(t, string(encoded), canonical)
			require.NotContains(t, string(encoded), `\n`)
		}
	})
}

func TestValidatePromptImagesModelGate(t *testing.T) {
	png := fixtureImageBase64(t, "valid.png")
	block := acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimePNG}}

	providersWith := func(image *bool) opencode.ProvidersResponse {
		return opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
			ID: "openai", Models: map[string]opencode.ProviderModel{
				"gpt-test": {ID: "gpt-test", Capabilities: &opencode.ProviderModelCapabilities{
					Input: opencode.ProviderModelInputCapabilities{Image: image},
				}},
			},
		}}}
	}

	t.Run("unsupported rejects", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.providers = providersWith(boolPtr(false))
		session := testSession(NewAgent(), client)
		requireInvalidParamsData(t, validatePromptMediaError(session, block), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorUnsupportedByModel,
			jsonFieldIndex: 0,
		})
	})

	t.Run("unsupported names the first raster block", func(t *testing.T) {
		pdfMime := "application/pdf"
		client := newFakeOpenCodeClient()
		client.providers = providersWith(boolPtr(false))
		session := testSession(NewAgent(), client)
		requireInvalidParamsData(t, validatePromptMediaError(session,
			blobResourceBlock(base64.StdEncoding.EncodeToString([]byte("%PDF-1.7")), &pdfMime),
			block,
		), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorUnsupportedByModel,
			jsonFieldIndex: 1,
		})
	})

	// A raster can arrive on a resource blob, and the model gate is the one
	// verdict that reports an index it did not compute on the spot. Naming
	// prompt.image for it would point the host at a block that is not there.
	t.Run("unsupported names the member the raster arrived on", func(t *testing.T) {
		pngMime := mimePNG
		client := newFakeOpenCodeClient()
		client.providers = providersWith(boolPtr(false))
		session := testSession(NewAgent(), client)
		requireInvalidParamsData(t, validatePromptMediaError(session,
			acp.TextBlock("look at this"),
			blobResourceBlock(fixtureImageBase64(t, "valid.png"), &pngMime),
		), map[string]any{
			jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorUnsupportedByModel,
			jsonFieldIndex: 0,
		})
	})

	t.Run("a document-only prompt never consults the image model gate", func(t *testing.T) {
		pdfMime := "application/pdf"
		client := newFakeOpenCodeClient()
		client.providers = providersWith(boolPtr(false))
		session := testSession(NewAgent(), client)
		require.NoError(t, validatePromptMediaError(session,
			blobResourceBlock(base64.StdEncoding.EncodeToString([]byte("%PDF-1.7")), &pdfMime),
		))
	})

	t.Run("supported forwards", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.providers = providersWith(boolPtr(true))
		session := testSession(NewAgent(), client)
		require.NoError(t, validatePromptMediaError(session, block))
	})

	t.Run("unknown catalog forwards", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		require.NoError(t, validatePromptMediaError(session, block))
	})

	t.Run("no images returns nil", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		require.NoError(t, validatePromptMediaError(session, acp.TextBlock("hi")))
	})
}

func TestSelectedModelImageSupportSources(t *testing.T) {
	t.Run("empty model is unknown", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		session.providerID = ""
		session.modelID = ""
		require.Equal(t, imageInputUnknown, session.selectedModelImageSupport(context.Background()))
	})

	t.Run("nil client is unknown", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		session.client = nil
		require.Equal(t, imageInputUnknown, session.selectedModelImageSupport(context.Background()))
	})

	t.Run("provider error is unknown", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.providersErr = errors.New("catalog down")
		session := testSession(NewAgent(), client)
		require.Equal(t, imageInputUnknown, session.selectedModelImageSupport(context.Background()))
	})

	t.Run("catalog record branches", func(t *testing.T) {
		require.Equal(t, imageInputUnknown, modelImageSupport(testProviders(), "openai/missing"))
		require.Equal(t, imageInputUnknown, modelImageSupport(testProviders(), "openai/gpt-test"))

		supported := opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
			ID: "openai", Models: map[string]opencode.ProviderModel{
				"gpt-test": {ID: "gpt-test", Capabilities: &opencode.ProviderModelCapabilities{
					Input: opencode.ProviderModelInputCapabilities{Image: boolPtr(true)},
				}},
			},
		}}}
		require.Equal(t, imageInputSupported, modelImageSupport(supported, "openai/gpt-test"))
	})
}

func TestPromptMediaBlocksCollectsEveryBlobResource(t *testing.T) {
	blobMime := "image/png"
	textMime := "text/plain"
	blocks := []acp.ContentBlock{
		acp.TextBlock("hello"),
		{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: mimePNG}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "BB==", MimeType: &blobMime},
		}}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "CC==", MimeType: &textMime},
		}}},
	}

	media := promptMediaBlocks(blocks)
	require.Len(t, media, 3)

	require.Equal(t, 0, media[0].index)
	require.Equal(t, 1, media[0].block)
	require.True(t, media[0].raster)
	require.Equal(t, "AA==", media[0].data)
	require.Equal(t, fieldPromptImage, media[0].field())

	require.Equal(t, 1, media[1].index)
	require.Equal(t, 2, media[1].block)
	require.True(t, media[1].raster)
	require.Equal(t, "BB==", media[1].data)

	require.Equal(t, 2, media[2].index)
	require.Equal(t, 3, media[2].block)
	require.False(t, media[2].raster)
	require.Equal(t, "CC==", media[2].data)
	require.Equal(t, fieldPromptResource, media[2].field())
}

func TestNormalizeMediaType(t *testing.T) {
	require.Equal(t, mimePNG, normalizeMediaType("  IMAGE/PNG  "))
	require.Equal(t, mimePNG, normalizeMediaType("Image/PNG; charset=binary"))
	require.Equal(t, "", normalizeMediaType(""))
	require.True(t, isImageMediaType("IMAGE/JPEG;q=1"))
	require.False(t, isImageMediaType("application/pdf"))
}

// TestValidatePromptMediaChargesTextResources pins the text resource variant to
// the same per-prompt budget as every other inbound byte channel, so bytes
// cannot escape the aggregate by arriving as text instead of as a blob.
func TestValidatePromptMediaChargesTextResources(t *testing.T) {
	textBlock := func(text, uri string) acp.ContentBlock {
		return acp.ContentBlock{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Text: text, Uri: uri},
		}}}
	}

	t.Run("text crosses the aggregate on its own", func(t *testing.T) {
		text := strings.Repeat("t", 1024)
		limit := int64(len(text)*2) - 1

		session := testSession(NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerPrompt: limit})), newFakeOpenCodeClient())
		requireInvalidParamsData(t, validatePromptMediaError(session,
			textBlock(text, "file:///tmp/notes"),
			textBlock(text, "file:///tmp/notes"),
		), map[string]any{
			jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 1,
			jsonFieldSizeBytes: int64(2 * len(text)), jsonFieldMaxBytes: limit,
		})
	})

	t.Run("text shares the budget with image bytes", func(t *testing.T) {
		png := fixtureImage(t, "valid.png")
		text := strings.Repeat("t", 64)
		limit := int64(len(png)+len(text)) - 1

		session := testSession(NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerPrompt: limit})), newFakeOpenCodeClient())
		requireInvalidParamsData(t, validatePromptMediaError(session,
			textBlock(text, ""),
			acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG, Data: base64.StdEncoding.EncodeToString(png)}},
		), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 1,
			jsonFieldSizeBytes: int64(len(png) + len(text)), jsonFieldMaxBytes: limit,
		})
	})

	t.Run("a resource with no text charges the uri it forwards", func(t *testing.T) {
		uri := "file:///tmp/" + strings.Repeat("u", 512)
		limit := int64(len(uri)) - 1

		session := testSession(NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerPrompt: limit})), newFakeOpenCodeClient())
		requireInvalidParamsData(t, validatePromptMediaError(session, textBlock("", uri)), map[string]any{
			jsonFieldField: fieldPromptResource, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
			jsonFieldSizeBytes: int64(len(uri)), jsonFieldMaxBytes: limit,
		})
	})

	t.Run("a charged text resource still maps to its unchanged native form", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		block := textBlock("notes", "file:///tmp/notes")
		require.NoError(t, validatePromptMediaError(session, block))

		parts, err := promptToOpenCodeParts([]acp.ContentBlock{block}, nil)
		require.NoError(t, err)
		require.Equal(t, map[string]any{jsonFieldType: partTypeText, partTypeText: "notes"}, parts[0])
	})
}

// TestValidatePromptImagesRejectsBytesPastTheTransportBound pins the frame
// bound as a gate rather than a retention limit: an image wider than a frame is
// refused whole, never trimmed to fit and forwarded as if it had been accepted.
func TestValidatePromptImagesRejectsBytesPastTheTransportBound(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	oversize := append(append([]byte(nil), png...), bytes.Repeat([]byte{0x41}, int(imageFrameBoundBytes)+1-len(png))...)

	// The per-image policy limit is disabled, so only the transport bound can
	// decide, and it is the bound the advertisement reports.
	session := testSession(NewAgent(WithImageLimits(ImageLimits{})), newFakeOpenCodeClient())
	block := acp.ContentBlock{Image: &acp.ContentBlockImage{
		Type: "image", MimeType: mimePNG, Data: base64.StdEncoding.EncodeToString(oversize),
	}}

	resolved, err := session.validatePromptMedia(context.Background(), []acp.ContentBlock{block})
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
		jsonFieldSizeBytes: imageFrameBoundBytes + 1, jsonFieldMaxBytes: imageFrameBoundBytes,
	})
	require.Empty(t, resolved)
}
