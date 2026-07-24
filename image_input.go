package opencodeacp

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	fieldPromptImage = "prompt.image"

	imageErrorMissingData        = "missing_data"
	imageErrorInvalidBase64      = "invalid_base64"
	imageErrorInvalidMediaType   = "invalid_media_type"
	imageErrorMediaTypeMismatch  = "media_type_mismatch"
	imageErrorAnimated           = "animated_not_supported"
	imageErrorInvalidDimensions  = "invalid_dimensions"
	imageErrorTooLarge           = "too_large"
	imageErrorUnsupportedByModel = "unsupported_by_model"
)

// imageInputAllowlist is the exact static input format contract: only these
// four canonical MIME strings are accepted, and animated members of the same
// containers are rejected structurally before a native turn starts.
var imageInputAllowlist = map[string]struct{}{
	mimePNG:  {},
	mimeJPEG: {},
	mimeGIF:  {},
	mimeWebP: {},
}

// imageInputSupport is the internal selected-model gate state sourced only
// from the authenticated provider catalog's capabilities.input.image field.
type imageInputSupport uint8

const (
	imageInputUnknown imageInputSupport = iota
	imageInputUnsupported
	imageInputSupported
)

// promptImage is one image-bearing prompt block: an image content block or an
// embedded blob resource whose MIME declares an image. Indexes are assigned
// across image-bearing blocks in request order and stay stable in errors.
type promptImage struct {
	index int
	mime  string
	data  string
}

func imageInputError(errValue string, index int) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldField: fieldPromptImage,
		jsonFieldError: errValue,
		jsonFieldIndex: index,
	})
}

func imageInputSizeError(index int, sizeBytes, maxBytes int64) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldField:     fieldPromptImage,
		jsonFieldError:     imageErrorTooLarge,
		jsonFieldIndex:     index,
		jsonFieldSizeBytes: sizeBytes,
		jsonFieldMaxBytes:  maxBytes,
	})
}

// promptImageBlocks collects the image-bearing blocks with stable indexes.
// The blob-resource predicate matches the native mapping exactly so every
// block that becomes a native image part is validated first.
func promptImageBlocks(blocks []acp.ContentBlock) []promptImage {
	images := make([]promptImage, 0, len(blocks))

	for _, block := range blocks {
		switch {
		case block.Image != nil:
			images = append(images, promptImage{index: len(images), mime: block.Image.MimeType, data: block.Image.Data})
		case block.Resource != nil && block.Resource.Resource.BlobResourceContents != nil:
			blob := block.Resource.Resource.BlobResourceContents
			if blob.MimeType != nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(*blob.MimeType)), mediaTypeImage+"/") {
				images = append(images, promptImage{index: len(images), mime: *blob.MimeType, data: blob.Blob})
			}
		}
	}

	return images
}

// validatePromptImages rejects invalid, unsupported, animated, or oversize
// prompt images before the native turn starts, deterministically at the first
// failing block in request order, then consults the selected-model gate.
func (s *session) validatePromptImages(ctx context.Context, blocks []acp.ContentBlock) error {
	images := promptImageBlocks(blocks)
	if len(images) == 0 {
		return nil
	}

	limits := s.imageLimits()

	var totalBytes int64

	for _, image := range images {
		size, err := validatePromptImage(image, limits)
		if err != nil {
			return err
		}

		totalBytes += size
		if limits.MaxInputBytesPerPrompt > 0 && totalBytes > limits.MaxInputBytesPerPrompt {
			return imageInputSizeError(image.index, totalBytes, limits.MaxInputBytesPerPrompt)
		}
	}

	if s.selectedModelImageSupport(ctx) == imageInputUnsupported {
		return acp.NewInvalidParams(map[string]any{
			jsonFieldField: fieldPromptImage,
			jsonFieldError: imageErrorUnsupportedByModel,
			jsonFieldIndex: images[0].index,
		})
	}

	return nil
}

// validatePromptImage runs the per-image pipeline and returns the decoded
// size: required data, canonical MIME allowlist, single decode, structural
// header/dimension walk, animation rejection, sniffed-versus-declared
// agreement, then the per-image decoded-byte limit.
func validatePromptImage(image promptImage, limits ImageLimits) (int64, error) {
	if image.data == "" {
		return 0, imageInputError(imageErrorMissingData, image.index)
	}

	if _, ok := imageInputAllowlist[image.mime]; !ok {
		return 0, imageInputError(imageErrorInvalidMediaType, image.index)
	}

	decoded, err := base64.StdEncoding.DecodeString(image.data)
	if err != nil {
		return 0, imageInputError(imageErrorInvalidBase64, image.index)
	}

	info, ok := inspectRaster(decoded)
	if !ok {
		// A recognized signature with an unreadable header is a dimensions
		// failure, which the gate order reports before a declared-versus-sniffed
		// mismatch; only unrecognized bytes fall through to the mismatch.
		if info.MIME == "" {
			return 0, imageInputError(imageErrorMediaTypeMismatch, image.index)
		}

		return 0, imageInputError(imageErrorInvalidDimensions, image.index)
	}

	if info.Animated {
		return 0, imageInputError(imageErrorAnimated, image.index)
	}

	if info.MIME != image.mime {
		return 0, imageInputError(imageErrorMediaTypeMismatch, image.index)
	}

	size := int64(len(decoded))
	if limits.MaxInputBytesPerImage > 0 && size > limits.MaxInputBytesPerImage {
		return 0, imageInputSizeError(image.index, size, limits.MaxInputBytesPerImage)
	}

	return size, nil
}

// selectedModelImageSupport consults the authenticated provider catalog's
// capabilities.input.image for the model selected at prompt time. Absent
// catalog data of any kind yields unknown, which forwards and leaves the
// native harness and provider authoritative.
func (s *session) selectedModelImageSupport(ctx context.Context) imageInputSupport {
	snapshot := s.snapshot()

	model := snapshot.modelValue()
	if model == "" || snapshot.client == nil {
		return imageInputUnknown
	}

	providers, err := snapshot.client.ConfigProviders(ctx)
	if err != nil {
		return imageInputUnknown
	}

	return modelImageSupport(providers, model)
}

func modelImageSupport(providers opencode.ProvidersResponse, model string) imageInputSupport {
	record, ok := providers.Model(model)
	if !ok || record.Capabilities == nil || record.Capabilities.Input.Image == nil {
		return imageInputUnknown
	}

	if *record.Capabilities.Input.Image {
		return imageInputSupported
	}

	return imageInputUnsupported
}
