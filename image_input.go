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

// promptMedia is one byte-bearing prompt block: an image content block, an
// embedded blob resource, or an embedded text resource. Indexes are assigned
// across these blocks in request order and stay stable in errors; block records
// the position in the prompt slice so validated bytes reach native mapping.
type promptMedia struct {
	index int
	block int
	// raster marks a block the four-format image contract governs: any image
	// content block, and a blob resource whose declared MIME normalizes under
	// the image prefix. Every other blob resource is gated for base64 validity
	// and decoded bytes only, leaving its native representation untouched.
	raster bool
	// text marks an embedded text resource. It carries no image contract and no
	// base64, but the characters it forwards are prompt bytes all the same, so
	// they are charged to the per-prompt aggregate.
	text  bool
	mime  string
	data  string
	image *acp.ContentBlockImage
}

// field names the request member a verdict belongs to. It follows the block
// the bytes arrived on, never the gate chain they were routed through: a
// resource block stays a resource verdict even when an image MIME sends it
// through the raster chain.
func (m promptMedia) field() string {
	if m.image != nil {
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

// promptMediaBlocks collects the byte-bearing blocks with stable indexes. The
// resource predicates match the native mapping exactly, including which variant
// wins when a resource carries both, so every block whose bytes can reach the
// harness is validated first.
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
		case block.Resource != nil && block.Resource.Resource.TextResourceContents != nil:
			contents := block.Resource.Resource.TextResourceContents

			media = append(media, promptMedia{
				index: len(media),
				block: position,
				text:  true,
				data:  firstNonEmpty(contents.Text, contents.Uri),
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
// returns the validated bytes native mapping sends for every image block, in
// either input form.
func (s *session) validatePromptMedia(ctx context.Context, blocks []acp.ContentBlock) (resolvedPromptMedia, error) {
	resolved := make(resolvedPromptMedia)

	media := promptMediaBlocks(blocks)
	if len(media) == 0 {
		return resolved, nil
	}

	limits := s.imageLimits()
	promptGate := effectiveInputBytesPerPrompt(limits.MaxInputBytesPerPrompt)

	// The selected-model gate answers for the first raster in the prompt, which
	// may have arrived on a resource blob rather than on an image block. Its
	// field is carried alongside its index so the verdict names the member the
	// bytes came in on, as every other media verdict does.
	var (
		totalBytes  int64
		handoffs    int
		firstRaster = -1
		rasterField string
	)

	for _, block := range media {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if isHandoffForm(block) {
			// Counted before the block is read, because bounding the reads is
			// the whole point of the cap.
			handoffs++
			if handoffs > maxHandoffBlocksPerPrompt {
				return nil, mediaInputSizeError(block.field(), block.index, int64(handoffs), maxHandoffBlocksPerPrompt)
			}
		}

		decoded, size, err := s.validatePromptMediaBlock(ctx, block, limits)
		if err != nil {
			return nil, err
		}

		if block.raster && firstRaster < 0 {
			firstRaster = block.index
			rasterField = block.field()
		}

		// The base64 native mapping forwards is re-encoded from the bytes the
		// gates measured, for a blob resource as much as for an image block: the
		// host's own spelling never reaches the harness, so a payload no gate
		// inspected cannot ride along with one that passed.
		if decoded != nil {
			resolved[block.block] = resolvedPromptBytes{
				mime: block.mime,
				data: base64.StdEncoding.EncodeToString(decoded),
			}
		}

		totalBytes += size
		if promptGate > 0 && totalBytes > promptGate {
			return nil, mediaInputSizeError(block.field(), block.index, totalBytes, promptGate)
		}
	}

	if firstRaster >= 0 && s.selectedModelImageSupport(ctx) == imageInputUnsupported {
		return nil, mediaInputError(rasterField, imageErrorUnsupportedByModel, firstRaster)
	}

	return resolved, nil
}

// isHandoffForm reports whether a block arrived in the handoff form: an image
// block carrying no embedded data that signals a local handoff reference.
func isHandoffForm(media promptMedia) bool {
	return media.image != nil && media.data == "" && handoffIntent(media.image)
}

// validatePromptMediaBlock resolves one block's decoded bytes and the size to
// charge against the byte gates. A handoff-form block resolves through the
// handoff pre-gate; every other block decodes its embedded base64. Either way
// the size charged is the length of the bytes the block contributes.
func (s *session) validatePromptMediaBlock(ctx context.Context, media promptMedia, limits ImageLimits) ([]byte, int64, error) {
	switch {
	case media.text:
		return nil, int64(len(media.data)), nil
	case isHandoffForm(media):
		decoded, err := s.readHandoffImage(ctx, media, limits)
		if err != nil {
			return nil, 0, err
		}

		size := int64(len(decoded))

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
	gate := effectiveInputBytesPerImage(limits.MaxInputBytesPerImage)
	if size > gate {
		return mediaInputSizeError(media.field(), media.index, size, gate)
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
