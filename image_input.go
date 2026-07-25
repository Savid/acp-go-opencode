package opencodeacp

import (
	"context"
	"encoding/base64"
	"slices"

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

	imageErrorInvalidHandoff = "invalid_handoff"
	imageErrorPathNotAllowed = "path_not_allowed"
	imageErrorMissingFile    = "missing_file"
	imageErrorDigestMismatch = "handoff_digest_mismatch"
)

// imageInputFormats is the exact static input format contract, in the order it
// is advertised: only these four canonical MIME strings are accepted, and
// animated members of the same containers are rejected structurally before a
// native turn starts.
var imageInputFormats = []string{mimePNG, mimeJPEG, mimeGIF, mimeWebP}

// imageInputSupport is the internal selected-model gate state sourced only
// from the authenticated provider catalog's capabilities.input.image field.
type imageInputSupport uint8

const (
	imageInputUnknown imageInputSupport = iota
	imageInputUnsupported
	imageInputSupported
)

// promptMedia is one media-bearing prompt block: an image content block or an
// embedded blob resource. Indexes are assigned across media-bearing blocks in
// request order and stay stable in errors; block records the position in the
// prompt slice so validated handoff bytes reach native mapping.
type promptMedia struct {
	index int
	block int
	// raster marks a block the four-format image contract governs: any image
	// content block, and a blob resource whose declared MIME normalizes under
	// the image prefix. Every other blob resource is gated for base64 validity
	// and decoded bytes only, leaving its native representation untouched.
	raster bool
	mime   string
	data   string
	image  *acp.ContentBlockImage
}

// field names the request member a verdict belongs to: the image contract for
// raster media, the resource channel for every other embedded blob.
func (m promptMedia) field() string {
	if m.raster {
		return fieldPromptImage
	}

	return fieldPromptResource
}

func mediaInputError(field, errValue string, index int) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldField: field,
		jsonFieldError: errValue,
		jsonFieldIndex: index,
	})
}

func mediaInputSizeError(field string, index int, sizeBytes, maxBytes int64) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldField:     field,
		jsonFieldError:     imageErrorTooLarge,
		jsonFieldIndex:     index,
		jsonFieldSizeBytes: sizeBytes,
		jsonFieldMaxBytes:  maxBytes,
	})
}

// handoffInputError reports a handoff pre-gate verdict. It carries the real
// cause as a human message and never the host-supplied path, which outlives
// the validation read nowhere.
func handoffInputError(index int, errValue, message string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldField:   fieldPromptImage,
		jsonFieldError:   errValue,
		jsonFieldIndex:   index,
		jsonFieldMessage: message,
	})
}

// promptMediaBlocks collects the media-bearing blocks with stable indexes. The
// blob-resource predicate matches the native mapping exactly, so every block
// whose bytes can reach the harness is validated first.
func promptMediaBlocks(blocks []acp.ContentBlock) []promptMedia {
	media := make([]promptMedia, 0, len(blocks))

	for position, block := range blocks {
		switch {
		case block.Image != nil:
			media = append(media, promptMedia{
				index:  len(media),
				block:  position,
				raster: true,
				mime:   block.Image.MimeType,
				data:   block.Image.Data,
				image:  block.Image,
			})
		case block.Resource != nil && block.Resource.Resource.BlobResourceContents != nil:
			blob := block.Resource.Resource.BlobResourceContents

			declared := ""
			if blob.MimeType != nil {
				declared = *blob.MimeType
			}

			media = append(media, promptMedia{
				index:  len(media),
				block:  position,
				raster: isImageMediaType(declared),
				mime:   declared,
				data:   blob.Blob,
			})
		}
	}

	return media
}

// validatePromptMedia rejects invalid, unsupported, animated, or oversize
// prompt media before the native turn starts, deterministically at the first
// failing block in request order, then consults the selected-model gate. It
// returns the handoff-form bytes native mapping substitutes for the blocks
// that carried no embedded data.
func (s *session) validatePromptMedia(ctx context.Context, blocks []acp.ContentBlock) (resolvedHandoffImages, error) {
	resolved := make(resolvedHandoffImages)

	media := promptMediaBlocks(blocks)
	if len(media) == 0 {
		return resolved, nil
	}

	limits := s.imageLimits()

	var (
		totalBytes int64
		firstImage = -1
	)

	for _, block := range media {
		decoded, size, err := s.validatePromptMediaBlock(block, limits)
		if err != nil {
			return nil, err
		}

		if block.raster && firstImage < 0 {
			firstImage = block.index
		}

		if block.image != nil && block.data == "" {
			resolved[block.block] = resolvedHandoffImage{
				mime: block.mime,
				data: base64.StdEncoding.EncodeToString(decoded),
			}
		}

		totalBytes += size
		if limits.MaxInputBytesPerPrompt > 0 && totalBytes > limits.MaxInputBytesPerPrompt {
			return nil, mediaInputSizeError(block.field(), block.index, totalBytes, limits.MaxInputBytesPerPrompt)
		}
	}

	if firstImage >= 0 && s.selectedModelImageSupport(ctx) == imageInputUnsupported {
		return nil, mediaInputError(fieldPromptImage, imageErrorUnsupportedByModel, firstImage)
	}

	return resolved, nil
}

// validatePromptMediaBlock resolves one block's decoded bytes and the size to
// charge against the byte gates. An image block with empty data that signals
// handoff intent resolves through the handoff pre-gate; every other block
// decodes its embedded base64.
func (s *session) validatePromptMediaBlock(media promptMedia, limits ImageLimits) ([]byte, int64, error) {
	switch {
	case media.image != nil && media.data == "" && handoffIntent(media.image):
		decoded, size, err := s.readHandoffImage(media, limits)
		if err != nil {
			return nil, 0, err
		}

		if err := checkMediaAllowlist(media); err != nil {
			return nil, 0, err
		}

		return decoded, size, validateRasterMedia(media, decoded, size, limits)
	case media.raster:
		if media.data == "" {
			return nil, 0, mediaInputError(media.field(), imageErrorMissingData, media.index)
		}

		if err := checkMediaAllowlist(media); err != nil {
			return nil, 0, err
		}

		decoded, err := decodeMediaBytes(media)
		if err != nil {
			return nil, 0, err
		}

		size := int64(len(decoded))

		return decoded, size, validateRasterMedia(media, decoded, size, limits)
	case media.data == "":
		return nil, 0, nil
	default:
		decoded, err := decodeMediaBytes(media)
		if err != nil {
			return nil, 0, err
		}

		size := int64(len(decoded))

		return decoded, size, checkMediaSize(media, size, limits)
	}
}

// checkMediaAllowlist enforces the canonical four-format input contract. It
// compares the declared MIME exactly, so a case or parameter variant is a
// media-type failure rather than a silently accepted raster.
func checkMediaAllowlist(media promptMedia) error {
	if !slices.Contains(imageInputFormats, media.mime) {
		return mediaInputError(media.field(), imageErrorInvalidMediaType, media.index)
	}

	return nil
}

func decodeMediaBytes(media promptMedia) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(media.data)
	if err != nil {
		return nil, mediaInputError(media.field(), imageErrorInvalidBase64, media.index)
	}

	return decoded, nil
}

// validateRasterMedia runs the structural image chain over decoded bytes: a
// header and dimension walk, animation rejection, sniffed-versus-declared
// agreement, then the per-image decoded-byte limit.
func validateRasterMedia(media promptMedia, decoded []byte, size int64, limits ImageLimits) error {
	info, ok := inspectRaster(decoded)
	if !ok {
		// A recognized signature with an unreadable header is a dimensions
		// failure, which the gate order reports before a declared-versus-sniffed
		// mismatch; only unrecognized bytes fall through to the mismatch.
		if info.MIME == "" {
			return mediaInputError(media.field(), imageErrorMediaTypeMismatch, media.index)
		}

		return mediaInputError(media.field(), imageErrorInvalidDimensions, media.index)
	}

	if info.Animated {
		return mediaInputError(media.field(), imageErrorAnimated, media.index)
	}

	if info.MIME != media.mime {
		return mediaInputError(media.field(), imageErrorMediaTypeMismatch, media.index)
	}

	return checkMediaSize(media, size, limits)
}

func checkMediaSize(media promptMedia, size int64, limits ImageLimits) error {
	if limits.MaxInputBytesPerImage > 0 && size > limits.MaxInputBytesPerImage {
		return mediaInputSizeError(media.field(), media.index, size, limits.MaxInputBytesPerImage)
	}

	return nil
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
