package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	imageOutputStage = "image_output"

	schemeHTTP  = "http"
	schemeHTTPS = "https"
	schemeFile  = "file"

	outputReasonInvalidBase64     = "invalid_base64"
	outputReasonNotARaster        = "not_a_raster"
	outputReasonMediaTypeMismatch = "media_type_mismatch"
	outputReasonMissingFile       = "missing_file"
	outputReasonPathNotAllowed    = "path_not_allowed"
	outputReasonTooLarge          = "too_large"
	outputReasonStorageFailed     = "storage_failed"

	// Guidance carried back in place of an image output the adapter will not
	// ship. Each string is a fixed constant keyed only by the verdict token: it
	// says what to do next and never describes the path, filename, size, or
	// operating-system error that produced the verdict.
	imageGuidancePathNotAllowed = "write the image inside the workspace and try again"
	imageGuidanceMissingFile    = "the image file could not be read; write the image inside the workspace and try again"
	imageGuidanceTooLarge       = "the image is too large to send; write a smaller image and try again"
	imageGuidanceNotRaster      = "the file is not a supported raster image; write a PNG, JPEG, GIF, WebP, BMP, ICO, or TIFF and try again"
	imageGuidanceInvalidBase64  = "the image payload could not be decoded; write the image to a file inside the workspace and try again"
	imageGuidanceMIMEMismatched = "the declared media type does not match the image; write the image again with a matching media type"

	// Wire messages for a refused local artifact. Like the guidance above they
	// are fixed: a refusal never names the path, filename, or byte count it
	// refused, so it cannot answer questions about the filesystem.
	outputMessageUnreadable   = "native image artifact is not readable"
	outputMessageOutsideRoots = "native image artifact resolves outside the allowed roots"
	outputMessageNotRegular   = "native image artifact is not a regular file"
	outputMessageMIMEMismatch = "native image artifact declares a media type its bytes contradict"
)

// imageOutputGuidance classifies an image-output failure. A recoverable
// verdict is an ordinary mistake that can be retried — the bytes were written
// somewhere the adapter may not read, are gone, are too big, or are not an
// image — and comes back with fixed guidance. A storage failure is the
// adapter's own artifact store breaking, which no retry addresses.
func imageOutputGuidance(err error) (string, bool) {
	var failure *acp.RequestError
	if !errors.As(err, &failure) {
		return "", false
	}

	data, _ := failure.Data.(map[string]any)
	if data[jsonFieldStage] != imageOutputStage {
		return "", false
	}

	reason, _ := data[jsonFieldReason].(string)

	switch reason {
	case outputReasonPathNotAllowed:
		return imageGuidancePathNotAllowed, true
	case outputReasonMissingFile:
		return imageGuidanceMissingFile, true
	case outputReasonTooLarge:
		return imageGuidanceTooLarge, true
	case outputReasonNotARaster:
		return imageGuidanceNotRaster, true
	case outputReasonInvalidBase64:
		return imageGuidanceInvalidBase64, true
	case outputReasonMediaTypeMismatch:
		return imageGuidanceMIMEMismatched, true
	default:
		return "", false
	}
}

// Filesystem seams for deterministic materialization fault tests.
var (
	imageEvalSymlinks = filepath.EvalSymlinks
	imageStat         = os.Stat
	imageOpen         = os.Open
	imageReadAll      = io.ReadAll
)

// outputComparableMIMEs are the declared MIME types the sniffer can verify.
// A declared type outside this set is replaced by the truthful sniffed MIME
// instead of being judged; output is not format-allowlisted.
var outputComparableMIMEs = map[string]struct{}{
	mimePNG:  {},
	mimeJPEG: {},
	mimeGIF:  {},
	mimeWebP: {},
	mimeBMP:  {},
	mimeICO:  {},
	mimeTIFF: {},
}

// imageOutputItem is one mapped native artifact: an embedded image block or
// a resource link for a remote-only location.
type imageOutputItem struct {
	block       acp.ContentBlock
	fingerprint string
	uri         string
	sizeBytes   int64
	isImage     bool
}

// key identifies the artifact for de-duplication inside one content array
// and across a turn's repeated lifecycle updates.
func (i imageOutputItem) key() string {
	if i.isImage {
		return "image:" + i.fingerprint
	}

	return "link:" + i.uri
}

// imageOutputFailure is the uniform turn-failure envelope for an adapter
// image representation failure: always turn-fatal, cause transport, with the
// machine-readable stage/reason fields and sizes only when truthful.
func imageOutputFailure(reason, message string, sizeBytes, maxBytes int64) error {
	data := turnFailedData(causeTransport, message, 0, "")
	data[jsonFieldStage] = imageOutputStage
	data[jsonFieldReason] = reason

	if sizeBytes > 0 {
		data[jsonFieldSizeBytes] = sizeBytes
	}

	if maxBytes > 0 {
		data[jsonFieldMaxBytes] = maxBytes
	}

	return acp.NewInternalError(data)
}

// nativeToolAttachments decodes the completed tool state's attachments array.
func nativeToolAttachments(state json.RawMessage) []opencode.NativeAttachment {
	var wrapper struct {
		Attachments []opencode.NativeAttachment `json:"attachments"`
	}

	_ = json.Unmarshal(state, &wrapper)

	return wrapper.Attachments
}

// parseImageDataURL splits a base64 data URL into its exact prefix (through
// the comma), MIME type, and payload.
func parseImageDataURL(value string) (prefix, mime, payload string, ok bool) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", "", false
	}

	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return "", "", "", false
	}

	meta := value[len("data:"):comma]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", "", false
	}

	mime = strings.TrimSuffix(meta, ";base64")
	if semicolon := strings.IndexByte(mime, ';'); semicolon >= 0 {
		mime = mime[:semicolon]
	}

	return value[:comma+1], mime, value[comma+1:], true
}

// mapOutputArtifact normalizes one native artifact to at most one content
// item. mapped reports whether the artifact has a representation on the image
// surface; errors are adapter representation failures and turn-fatal.
func (s *session) mapOutputArtifact(
	ctx context.Context,
	artifact opencode.NativeAttachment,
	identity string,
	provenance string,
	replay bool,
) (imageOutputItem, bool, error) {
	image := isImageMediaType(artifact.Mime)

	switch {
	case artifact.URL == "":
		if !image {
			return imageOutputItem{}, false, nil
		}

		return imageOutputItem{}, false, imageOutputFailure(outputReasonMissingFile, "native image artifact has no location or bytes", 0, 0)
	case strings.HasPrefix(artifact.URL, "data:"):
		if !image {
			return imageOutputItem{}, false, nil
		}

		return s.mapInlineImageArtifact(ctx, artifact, identity, provenance)
	case remoteArtifactURL(artifact.URL):
		return resourceLinkItem(artifact), true, nil
	case localArtifactPath(artifact.URL) != "":
		if !image {
			return imageOutputItem{}, false, nil
		}

		return s.mapLocalImageArtifact(ctx, artifact, identity, provenance, replay)
	default:
		if !image {
			return imageOutputItem{}, false, nil
		}

		return imageOutputItem{}, false, imageOutputFailure(outputReasonMissingFile, "native image artifact has an unusable location", 0, 0)
	}
}

func remoteArtifactURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}

	return parsed.Scheme == schemeHTTP || parsed.Scheme == schemeHTTPS
}

// localArtifactPath resolves a file URL or bare absolute path to a local
// filesystem path, or empty when the value is neither.
func localArtifactPath(value string) string {
	if filepath.IsAbs(value) {
		return value
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != schemeFile || parsed.Path == "" {
		return ""
	}

	return parsed.Path
}

func resourceLinkItem(artifact opencode.NativeAttachment) imageOutputItem {
	name := artifact.Filename
	if name == "" {
		name = filenameFromURI(artifact.URL)
	}

	if name == "" {
		name = artifact.URL
	}

	link := acp.ResourceLinkBlock(name, artifact.URL)
	if artifact.Mime != "" {
		mime := artifact.Mime
		link.ResourceLink.MimeType = &mime
	}

	return imageOutputItem{block: link, uri: artifact.URL}
}

func (s *session) mapInlineImageArtifact(
	ctx context.Context,
	artifact opencode.NativeAttachment,
	identity string,
	provenance string,
) (imageOutputItem, bool, error) {
	prefix, declaredMime, payload, ok := parseImageDataURL(artifact.URL)
	if !ok {
		return imageOutputItem{}, false, imageOutputFailure(outputReasonInvalidBase64, "native image artifact carries a malformed data URL", 0, 0)
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return imageOutputItem{}, false, imageOutputFailure(outputReasonInvalidBase64, "native image artifact data URL is not valid base64", 0, 0)
	}

	if declaredMime == "" {
		declaredMime = artifact.Mime
	}

	item, err := s.finishImageArtifact(ctx, decoded, payload, prefix, declaredMime, artifact.Filename, identity, provenance)
	if err != nil {
		return imageOutputItem{}, false, err
	}

	return item, true, nil
}

func (s *session) mapLocalImageArtifact(
	ctx context.Context,
	artifact opencode.NativeAttachment,
	identity string,
	provenance string,
	replay bool,
) (imageOutputItem, bool, error) {
	if replay {
		record, ok := s.imageArtifactByIdentity(identity)
		if !ok {
			return imageOutputItem{}, false, imageOutputFailure(
				outputReasonStorageFailed,
				"stored image artifact is no longer available for replay",
				0, 0,
			)
		}

		decoded, err := base64.StdEncoding.DecodeString(record.Data)
		if err != nil {
			return imageOutputItem{}, false, imageOutputFailure(outputReasonStorageFailed, "image artifact record failed integrity validation", 0, 0)
		}

		item, err := s.finishImageArtifact(ctx, decoded, record.Data, "", record.Mime, record.Filename, identity, provenance)
		if err != nil {
			return imageOutputItem{}, false, err
		}

		return item, true, nil
	}

	decoded, err := s.materializeLocalImage(localArtifactPath(artifact.URL))
	if err != nil {
		return imageOutputItem{}, false, err
	}

	payload := base64.StdEncoding.EncodeToString(decoded)

	item, err := s.finishImageArtifact(ctx, decoded, payload, "", artifact.Mime, artifact.Filename, identity, provenance)
	if err != nil {
		return imageOutputItem{}, false, err
	}

	return item, true, nil
}

// finishImageArtifact validates decoded bytes, enforces the per-image limit,
// registers the canonical artifact, and builds the embedded image block with
// the truthful sniffed MIME.
func (s *session) finishImageArtifact(
	ctx context.Context,
	decoded []byte,
	payload string,
	dataURLPrefix string,
	declaredMime string,
	filename string,
	identity string,
	provenance string,
) (imageOutputItem, error) {
	sniffed, err := validateOutputImageBytes(decoded, declaredMime)
	if err != nil {
		return imageOutputItem{}, err
	}

	size := int64(len(decoded))

	limit := effectiveOutputLimit(s.imageLimits().MaxOutputBytesPerImage)
	if size > limit {
		return imageOutputItem{}, imageOutputFailure(
			outputReasonTooLarge,
			"image output exceeds the per-image limit",
			size, limit,
		)
	}

	record := imageArtifactRecord{
		Version:            imageArtifactRecordVersion,
		NativeID:           identity,
		Provenance:         provenance,
		Fingerprint:        imageFingerprint(decoded),
		Mime:               sniffed,
		DataURLPrefix:      dataURLPrefix,
		Data:               payload,
		Filename:           filename,
		CreatedAtUnixMilli: imageArtifactNow().UnixMilli(),
	}
	if err := s.registerImageArtifact(ctx, identity, record); err != nil {
		return imageOutputItem{}, err
	}

	return imageOutputItem{
		block:       acp.ImageBlock(payload, sniffed),
		fingerprint: record.Fingerprint,
		sizeBytes:   size,
		isImage:     true,
	}, nil
}

// validateOutputImageBytes sniffs the artifact and returns the truthful MIME.
// Output has no format allowlist: any recognized raster is emitted, and a
// declared type the sniffer can verify must agree with the bytes.
func validateOutputImageBytes(decoded []byte, declaredMime string) (string, error) {
	sniffed, ok := sniffRasterMIME(decoded)
	if !ok {
		return "", imageOutputFailure(outputReasonNotARaster, "native image artifact bytes are not a recognizable raster", 0, 0)
	}

	declared := strings.ToLower(strings.TrimSpace(declaredMime))
	if _, verifiable := outputComparableMIMEs[declared]; verifiable && declared != sniffed {
		return "", imageOutputFailure(outputReasonMediaTypeMismatch, outputMessageMIMEMismatch, 0, 0)
	}

	return sniffed, nil
}

// allowedImageRoots are the only roots a harness-returned artifact path may
// resolve into: the session workspace directories, the wrapper-owned scratch
// parent, and the operating-system temporary directory the harness sandbox
// already writes to. The configured native home is never readable as a whole.
// The roots stop accidental reads of arbitrary paths; they are not an
// exfiltration boundary against a shell-capable agent, which can copy any
// readable file into the workspace.
func (s *session) allowedImageRoots() []string {
	roots := make([]string, 0, len(s.additionalDirectories)+3)
	roots = append(roots, s.cwd)
	roots = append(roots, s.additionalDirectories...)

	if s.agent != nil {
		roots = append(roots, scratchParent(s.agent.options.ScratchDir))
	}

	// The harness sandbox already permits writing to the OS temp directory, so
	// a root set without it refuses reads of files the model was allowed to
	// create. A temp file stays subject to every check in
	// materializeLocalImage: the temp directory is shared with every process on
	// the host, and nothing in it is trusted for being there.
	roots = append(roots, os.TempDir())

	return roots
}

// materializeLocalImage reads a harness-returned artifact path with
// symlink-safe resolution, regular-file and allowed-root checks, and a read
// bounded before allocating the declared size.
func (s *session) materializeLocalImage(path string) ([]byte, error) {
	resolved, err := imageEvalSymlinks(path)
	if err != nil {
		return nil, imageOutputFailure(outputReasonMissingFile, outputMessageUnreadable, 0, 0)
	}

	if !s.imagePathAllowed(resolved) {
		return nil, imageOutputFailure(outputReasonPathNotAllowed, outputMessageOutsideRoots, 0, 0)
	}

	info, err := imageStat(resolved)
	if err != nil {
		return nil, imageOutputFailure(outputReasonMissingFile, outputMessageUnreadable, 0, 0)
	}

	if !info.Mode().IsRegular() {
		return nil, imageOutputFailure(outputReasonPathNotAllowed, outputMessageNotRegular, 0, 0)
	}

	maxBytes := effectiveOutputLimit(s.imageLimits().MaxOutputBytesPerImage)
	if info.Size() > maxBytes {
		return nil, imageOutputFailure(outputReasonTooLarge, "image output exceeds the per-image limit", info.Size(), maxBytes)
	}

	file, err := imageOpen(resolved)
	if err != nil {
		return nil, imageOutputFailure(outputReasonMissingFile, outputMessageUnreadable, 0, 0)
	}
	defer file.Close()

	decoded, err := imageReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, imageOutputFailure(outputReasonMissingFile, outputMessageUnreadable, 0, 0)
	}

	if int64(len(decoded)) > maxBytes {
		return nil, imageOutputFailure(outputReasonTooLarge, "image output exceeds the per-image limit", int64(len(decoded)), maxBytes)
	}

	return decoded, nil
}

func (s *session) imagePathAllowed(resolved string) bool {
	for _, root := range s.allowedImageRoots() {
		resolvedRoot, err := imageEvalSymlinks(root)
		if err != nil {
			continue
		}

		if pathWithinRoot(resolvedRoot, resolved) {
			return true
		}
	}

	return false
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// toolContentSnapshot maps a completed tool state's attachments and merges
// them with everything already delivered for the tool call, so every
// content-bearing update carries the complete current array under ACP v1
// replace semantics. The merged array is bounded by the per-tool-call
// aggregate limit, attributed to the artifact that crosses it.
func (s *session) toolContentSnapshot(
	ctx context.Context,
	toolCallID string,
	part opencode.NativePart,
	attachments []opencode.NativeAttachment,
	replay bool,
) ([]imageOutputItem, bool, error) {
	previous := s.emittedToolContent[toolCallID]

	merged := make([]imageOutputItem, len(previous))
	copy(merged, previous)

	seen := make(map[string]struct{}, len(merged))
	for _, item := range merged {
		seen[item.key()] = struct{}{}
	}

	changed := false

	for index, attachment := range attachments {
		identity := toolAttachmentIdentity(part, attachment, index)

		item, mapped, err := s.mapOutputArtifact(ctx, attachment, identity, provenanceTool, replay)
		if err != nil {
			return nil, false, err
		}

		if !mapped {
			continue
		}

		if _, duplicate := seen[item.key()]; duplicate {
			continue
		}

		seen[item.key()] = struct{}{}

		merged = append(merged, item)
		changed = true
	}

	limit := effectiveOutputLimit(s.imageLimits().MaxOutputBytesPerToolCall)

	var total int64

	for _, item := range merged {
		if !item.isImage {
			continue
		}

		total += item.sizeBytes
		if total > limit {
			return nil, false, imageOutputFailure(
				outputReasonTooLarge,
				"tool call image content exceeds the per-tool-call limit",
				total, limit,
			)
		}
	}

	return merged, changed, nil
}

func toolAttachmentIdentity(part opencode.NativePart, attachment opencode.NativeAttachment, index int) string {
	suffix := attachment.ID
	if suffix == "" {
		suffix = fmt.Sprintf("%d", index)
	}

	return firstNonEmpty(part.CallID, part.ID) + "/" + suffix
}

func toolCallContentFromItems(items []imageOutputItem) []acp.ToolCallContent {
	content := make([]acp.ToolCallContent, 0, len(items))
	for _, item := range items {
		content = append(content, acp.ToolContent(item.block))
	}

	return content
}
