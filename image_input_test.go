package opencodeacp

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

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
			name: "blob resource case-variant media type",
			block: acp.ContentBlock{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
				BlobResourceContents: &acp.BlobResourceContents{Blob: png, MimeType: stringPtr("IMAGE/PNG")},
			}}},
			want: map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorInvalidMediaType, jsonFieldIndex: 0},
		},
		{
			name: "blob resource leading-whitespace media type",
			block: acp.ContentBlock{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
				BlobResourceContents: &acp.BlobResourceContents{Blob: png, MimeType: stringPtr(" image/png")},
			}}},
			want: map[string]any{jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorInvalidMediaType, jsonFieldIndex: 0},
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
			requireInvalidParamsData(t, session.validatePromptImages(context.Background(), []acp.ContentBlock{tt.block}), tt.want)
		})
	}
}

func TestValidatePromptImagesSizeLimits(t *testing.T) {
	png := fixtureImageBase64(t, "valid.png")
	decodedSize := int64(len(fixtureImage(t, "valid.png")))

	t.Run("per image", func(t *testing.T) {
		session := testSession(NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerImage: 1})), newFakeOpenCodeClient())
		requireInvalidParamsData(t, session.validatePromptImages(context.Background(), []acp.ContentBlock{
			{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimePNG}},
		}), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
			jsonFieldSizeBytes: decodedSize, jsonFieldMaxBytes: int64(1),
		})
	})

	t.Run("per prompt aggregate", func(t *testing.T) {
		session := testSession(NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerPrompt: decodedSize + 1})), newFakeOpenCodeClient())
		err := session.validatePromptImages(context.Background(), []acp.ContentBlock{
			{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimePNG}},
			{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimePNG}},
		})
		requireInvalidParamsData(t, err, map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 1,
			jsonFieldSizeBytes: 2 * decodedSize, jsonFieldMaxBytes: decodedSize + 1,
		})
	})

	t.Run("zero disables input limits", func(t *testing.T) {
		session := testSession(NewAgent(WithImageLimits(ImageLimits{})), newFakeOpenCodeClient())
		require.NoError(t, session.validatePromptImages(context.Background(), []acp.ContentBlock{
			{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: mimePNG}},
		}))
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
		requireInvalidParamsData(t, session.validatePromptImages(context.Background(), []acp.ContentBlock{block}), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorUnsupportedByModel,
			jsonFieldIndex: 0,
		})
	})

	t.Run("supported forwards", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.providers = providersWith(boolPtr(true))
		session := testSession(NewAgent(), client)
		require.NoError(t, session.validatePromptImages(context.Background(), []acp.ContentBlock{block}))
	})

	t.Run("unknown catalog forwards", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		require.NoError(t, session.validatePromptImages(context.Background(), []acp.ContentBlock{block}))
	})

	t.Run("no images returns nil", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		require.NoError(t, session.validatePromptImages(context.Background(), []acp.ContentBlock{acp.TextBlock("hi")}))
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

func TestPromptImageBlocksCollectsBlobResource(t *testing.T) {
	blobMime := "image/png"
	textMime := "text/plain"
	blocks := []acp.ContentBlock{
		{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: mimePNG}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "BB==", MimeType: &blobMime},
		}}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "CC==", MimeType: &textMime},
		}}},
	}

	images := promptImageBlocks(blocks)
	require.Len(t, images, 2)
	require.Equal(t, 0, images[0].index)
	require.Equal(t, "AA==", images[0].data)
	require.Equal(t, 1, images[1].index)
	require.Equal(t, "BB==", images[1].data)
}
