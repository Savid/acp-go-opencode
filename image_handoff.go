package opencodeacp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	// handoffEnvelopeKey is the family-reserved content-block metadata key that
	// carries a local handoff reference. Its object is versioned and holds
	// exactly the three fields below; anything else is rejected.
	handoffEnvelopeKey     = "acp-go.dev/handoff"
	handoffEnvelopeVersion = 1

	handoffFieldVersion   = "version"
	handoffFieldDigest    = "digest"
	handoffFieldSizeBytes = "sizeBytes"

	handoffDigestLength = 64
	handoffLocalHost    = "localhost"
)

// Handoff pre-gate causes. Each names the real defect and never the
// host-supplied path.
const (
	handoffCauseRootUnset      = "no input handoff root is configured"
	handoffCauseEnvelopeAbsent = "handoff envelope is absent"
	handoffCauseEnvelopeShape  = "handoff envelope is not a decodable object"
	handoffCauseUnknownField   = "handoff envelope carries an unrecognized field"
	handoffCauseVersion        = "handoff envelope version is absent or unsupported"
	handoffCauseDigest         = "handoff envelope digest is not 64 lowercase hexadecimal characters"
	handoffCauseSizeBytes      = "handoff envelope sizeBytes is absent, fractional, or negative"
	handoffCauseURI            = "handoff block URI is not an absolute local file URI"

	handoffCauseRootUnresolved = "the configured input handoff root does not resolve"
	handoffCauseOutsideRoot    = "handoff path lies outside the configured input handoff root"
	handoffCauseEscapesRoot    = "handoff path escapes the configured input handoff root"
	handoffCauseNotRegular     = "handoff path is not a regular file"

	handoffCauseAbsent        = "handoff file does not exist"
	handoffCauseUninspectable = "handoff file cannot be inspected"
	handoffCauseUnopenable    = "handoff file cannot be opened"
	handoffCauseUnreadable    = "handoff file cannot be read"

	handoffCauseSizeMismatch   = "handoff file size does not match the declared sizeBytes"
	handoffCauseDigestMismatch = "handoff file bytes do not hash to the declared digest"
)

// validateInputHandoffRoot requires an absolute read root when one is
// configured. An empty value leaves the handoff input form unavailable.
func validateInputHandoffRoot(dir string) error {
	if dir == "" || filepath.IsAbs(dir) {
		return nil
	}

	return errors.New("input handoff root must be an absolute path")
}

// handoffReference is a validated handoff envelope paired with the local path
// its block URI names.
type handoffReference struct {
	path      string
	digest    string
	sizeBytes int64
}

// resolvedHandoffImage carries one handoff-form image's validated bytes as the
// base64 payload native mapping substitutes for the block's empty data. The
// host-supplied handoff path is deliberately absent: it never reaches the
// native request.
type resolvedHandoffImage struct {
	mime string
	data string
}

// resolvedHandoffImages indexes validated handoff-form images by their
// position in the prompt block slice that validation and native mapping both
// walk.
type resolvedHandoffImages map[int]resolvedHandoffImage

// imagePart maps one image block to its native file part, substituting
// validated handoff bytes when the block arrived in the handoff form. A
// handoff part carries no filename because the only name available is the
// host's handoff path, which never crosses into a native request.
func (r resolvedHandoffImages) imagePart(index int, image *acp.ContentBlockImage) map[string]any {
	handoff, ok := r[index]
	if !ok {
		return imageOpenCodePart(image)
	}

	return map[string]any{
		jsonFieldType: partTypeFile,
		jsonFieldMime: handoff.mime,
		jsonFieldURL:  "data:" + handoff.mime + ";base64," + handoff.data,
	}
}

// handoffIntent reports whether an image block with empty data is attempting
// the handoff form. Intent is a handoff envelope key or a file URI; a block
// carrying neither is the embedded form's missing data.
func handoffIntent(image *acp.ContentBlockImage) bool {
	if _, ok := image.Meta[handoffEnvelopeKey]; ok {
		return true
	}

	if image.Uri == nil {
		return false
	}

	parsed, err := url.Parse(*image.Uri)

	return err == nil && parsed.Scheme == schemeFile
}

// readHandoffImage runs the handoff pre-gate ahead of every embedded gate:
// envelope shape, URI shape, bounded symlink-safe containment inside the
// configured read root, a regular file, a bounded read, then a fail-closed
// digest verification. The returned size is the on-disk size when the file
// overran the bound, because the digest is unverifiable then and the byte gate
// must reject the file on its real size.
func (s *session) readHandoffImage(media promptMedia, limits ImageLimits) ([]byte, int64, error) {
	root := s.inputHandoffRoot()
	if root == "" {
		return nil, 0, handoffInputError(media.index, imageErrorInvalidHandoff, handoffCauseRootUnset)
	}

	reference, cause := handoffReferenceFrom(media.image)
	if cause != "" {
		return nil, 0, handoffInputError(media.index, imageErrorInvalidHandoff, cause)
	}

	resolved, onDisk, err := resolveHandoffPath(root, reference.path, media.index)
	if err != nil {
		return nil, 0, err
	}

	bound := handoffReadBound(limits)

	decoded, err := readHandoffBytes(resolved, bound, media.index)
	if err != nil {
		return nil, 0, err
	}

	read := int64(len(decoded))
	if read > bound {
		// Past the bound the digest cannot be verified, so the bytes must be
		// rejected rather than forwarded. A configured per-image gate rejects
		// them on the file's real size once the structural chain has run; a
		// disabled one leaves the clamp as the only bound at fault.
		if limits.MaxInputBytesPerImage > 0 {
			return decoded, onDisk, nil
		}

		return nil, 0, mediaInputSizeError(media.field(), media.index, onDisk, bound)
	}

	if read != reference.sizeBytes {
		return nil, 0, handoffInputError(media.index, imageErrorDigestMismatch, handoffCauseSizeMismatch)
	}

	if handoffDigest(decoded) != reference.digest {
		return nil, 0, handoffInputError(media.index, imageErrorDigestMismatch, handoffCauseDigestMismatch)
	}

	return decoded, read, nil
}

func (s *session) inputHandoffRoot() string {
	if s.agent == nil {
		return ""
	}

	return s.agent.options.InputHandoffRoot
}

// handoffReferenceFrom decodes and validates the block's handoff envelope and
// URI. A non-empty cause reports the first defect found, in envelope order.
func handoffReferenceFrom(image *acp.ContentBlockImage) (handoffReference, string) {
	raw, present := image.Meta[handoffEnvelopeKey]
	if !present {
		return handoffReference{}, handoffCauseEnvelopeAbsent
	}

	fields, ok := handoffEnvelopeFields(raw)
	if !ok {
		return handoffReference{}, handoffCauseEnvelopeShape
	}

	for name := range fields {
		if name != handoffFieldVersion && name != handoffFieldDigest && name != handoffFieldSizeBytes {
			return handoffReference{}, handoffCauseUnknownField
		}
	}

	version, ok := handoffInteger(fields[handoffFieldVersion])
	if !ok || version != handoffEnvelopeVersion {
		return handoffReference{}, handoffCauseVersion
	}

	var digest string
	if err := json.Unmarshal(fields[handoffFieldDigest], &digest); err != nil || !handoffDigestSyntax(digest) {
		return handoffReference{}, handoffCauseDigest
	}

	sizeBytes, ok := handoffInteger(fields[handoffFieldSizeBytes])
	if !ok || sizeBytes < 0 {
		return handoffReference{}, handoffCauseSizeBytes
	}

	path, ok := handoffPathFromURI(image.Uri)
	if !ok {
		return handoffReference{}, handoffCauseURI
	}

	return handoffReference{path: path, digest: digest, sizeBytes: sizeBytes}, ""
}

// handoffEnvelopeFields re-encodes the metadata value and reads it back as a
// JSON object so envelope strictness does not depend on how the value reached
// the map.
func handoffEnvelopeFields(raw any) (map[string]json.RawMessage, bool) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil || fields == nil {
		return nil, false
	}

	return fields, true
}

// handoffInteger reads an exact JSON integer, rejecting absent values,
// fractional numbers, and values that merely spell a number in a string. The
// token check is required because a quoted numeral decodes into a JSON number
// without complaint.
func handoffInteger(raw json.RawMessage) (int64, bool) {
	token := bytes.TrimSpace(raw)
	if len(token) == 0 || (token[0] != '-' && (token[0] < '0' || token[0] > '9')) {
		return 0, false
	}

	value, err := json.Number(token).Int64()
	if err != nil {
		return 0, false
	}

	return value, true
}

func handoffDigestSyntax(value string) bool {
	if len(value) != handoffDigestLength {
		return false
	}

	return strings.IndexFunc(value, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) < 0
}

func handoffDigest(decoded []byte) string {
	sum := sha256.Sum256(decoded)

	return hex.EncodeToString(sum[:])
}

// handoffPathFromURI converts a block URI to a local path. Only a file URI
// with an empty or loopback host and an absolute path is usable.
func handoffPathFromURI(raw *string) (string, bool) {
	if raw == nil || *raw == "" {
		return "", false
	}

	parsed, err := url.Parse(*raw)
	if err != nil || parsed.Scheme != schemeFile {
		return "", false
	}

	if parsed.Host != "" && parsed.Host != handoffLocalHost {
		return "", false
	}

	if !strings.HasPrefix(parsed.Path, "/") {
		return "", false
	}

	return filepath.FromSlash(parsed.Path), true
}

// resolveHandoffPath binds a handoff path to the configured read root:
// lexical containment on the cleaned path, symlink resolution, containment
// again on the resolved path, then a regular-file check. It returns the
// resolved path and its on-disk size.
func resolveHandoffPath(root, path string, index int) (string, int64, error) {
	resolvedRoot, err := imageEvalSymlinks(root)
	if err != nil {
		return "", 0, handoffInputError(index, imageErrorPathNotAllowed, handoffCauseRootUnresolved)
	}

	cleaned := filepath.Clean(path)
	if !pathWithinRoot(filepath.Clean(root), cleaned) {
		return "", 0, handoffInputError(index, imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	}

	resolved, err := imageEvalSymlinks(cleaned)
	if err != nil {
		return "", 0, handoffInputError(index, imageErrorMissingFile, handoffCauseAbsent)
	}

	if !pathWithinRoot(resolvedRoot, resolved) {
		return "", 0, handoffInputError(index, imageErrorPathNotAllowed, handoffCauseEscapesRoot)
	}

	info, err := imageStat(resolved)
	if err != nil {
		return "", 0, handoffInputError(index, imageErrorMissingFile, handoffCauseUninspectable)
	}

	if !info.Mode().IsRegular() {
		return "", 0, handoffInputError(index, imageErrorPathNotAllowed, handoffCauseNotRegular)
	}

	return resolved, info.Size(), nil
}

// handoffReadBound is the largest handoff file the adapter will hold. It is the
// per-image gate when one is configured, and the decoded frame clamp when that
// policy limit is disabled, so a disabled byte policy never means an unbounded
// local read.
func handoffReadBound(limits ImageLimits) int64 {
	if limits.MaxInputBytesPerImage > 0 {
		return limits.MaxInputBytesPerImage
	}

	return imageFrameBoundBytes
}

// readHandoffBytes reads at most one byte past the bound, so an oversize file
// is detected without being held.
func readHandoffBytes(resolved string, maxBytes int64, index int) ([]byte, error) {
	file, err := imageOpen(resolved)
	if err != nil {
		return nil, handoffInputError(index, imageErrorMissingFile, handoffCauseUnopenable)
	}
	defer file.Close()

	decoded, err := imageReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, handoffInputError(index, imageErrorMissingFile, handoffCauseUnreadable)
	}

	return decoded, nil
}
