package opencodeacp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
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

	// maxHandoffBlocksPerPrompt bounds how many handoff-form blocks one prompt
	// may read. A prompt frame can name far more local files than it could ever
	// carry bytes for, so the block count is what bounds the adapter's local
	// I/O when the per-prompt byte aggregate is disabled.
	maxHandoffBlocksPerPrompt = 64
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

	handoffCauseRootUnresolved = "the configured input handoff root cannot be opened"
	handoffCauseOutsideRoot    = "handoff path does not open inside the configured input handoff root"
	handoffCauseNotRegular     = "handoff path is not a regular file"

	handoffCauseAbsent     = "handoff file does not exist"
	handoffCauseUnreadable = "handoff file cannot be read"

	// handoffCauseDigestMismatch covers a byte count and a hash that disagree
	// with the envelope alike: naming which of the two failed would report an
	// observation about a file the caller only guessed at.
	handoffCauseDigestMismatch = "handoff file does not match the declared envelope"
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

// resolvedPromptImage carries one validated image's bytes re-encoded from the
// bytes validation actually inspected, which is what native mapping sends. A
// host's own base64 spelling never reaches the harness, so two spellings of
// the same image cannot become two different native requests, and neither form
// contributes a name derived from a block URI.
type resolvedPromptImage struct {
	mime string
	data string
}

// resolvedPromptImages indexes validated images by their position in the
// prompt block slice that validation and native mapping both walk.
type resolvedPromptImages map[int]resolvedPromptImage

// imagePart maps one image block to its native file part from the validated
// bytes recorded for it.
func (r resolvedPromptImages) imagePart(index int, image *acp.ContentBlockImage) map[string]any {
	validated, ok := r[index]
	if !ok {
		return imageOpenCodePart(image)
	}

	return map[string]any{
		jsonFieldType: partTypeFile,
		jsonFieldMime: validated.mime,
		jsonFieldURL:  "data:" + validated.mime + ";base64," + validated.data,
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
// envelope shape, URI shape, an open the read root confines, the declared media
// type, the declared size, a bounded read, then a fail-closed digest
// verification. Every byte it returns has been verified against the declared
// digest.
func (s *session) readHandoffImage(ctx context.Context, media promptMedia, limits ImageLimits) ([]byte, error) {
	dir := s.inputHandoffRoot()
	if dir == "" {
		return nil, handoffInputError(media.index, imageErrorInvalidHandoff, handoffCauseRootUnset)
	}

	reference, cause := handoffReferenceFrom(media.image)
	if cause != "" {
		return nil, handoffInputError(media.index, imageErrorInvalidHandoff, cause)
	}

	// The open and the read are the only syscalls a hung filesystem can park
	// this turn on, so a cancellation already in hand ends the block here.
	if cancelled := ctx.Err(); cancelled != nil {
		return nil, cancelled
	}

	file, err := handoffOpen(dir, reference.path, media.index)
	if err != nil {
		return nil, err
	}

	// Every gate below returns, so the descriptor is released here rather than
	// on each refusal path.
	defer file.Close()

	// The declared media type decides before any bytes are read, so a block
	// outside the format contract costs one open rather than a read and a hash.
	if refused := checkMediaAllowlist(media); refused != nil {
		return nil, refused
	}

	gate := effectiveInputBytesPerImage(limits.MaxInputBytesPerImage)
	if reference.sizeBytes > gate {
		return nil, mediaInputSizeError(media.field(), media.index, reference.sizeBytes, gate)
	}

	decoded, err := imageReadAll(io.LimitReader(file, gate+1))
	if err != nil {
		return nil, handoffInputError(media.index, imageErrorMissingFile, handoffCauseUnreadable)
	}

	// The size verdict is decided on the bytes read and on nothing else, so a
	// file that grew while it was being read is rejected rather than forwarded.
	if int64(len(decoded)) > gate {
		return nil, mediaInputSizeError(media.field(), media.index, int64(len(decoded)), gate)
	}

	if int64(len(decoded)) != reference.sizeBytes || !handoffDigestMatches(decoded, reference.digest) {
		return nil, handoffInputError(media.index, imageErrorDigestMismatch, handoffCauseDigestMismatch)
	}

	return decoded, nil
}

// handoffOpen is the seam the handoff read opens through, so a test can observe
// that every descriptor handed to the gate chain is released again.
var handoffOpen = openHandoffFile

// openHandoffFile opens one handoff reference inside the configured read root.
// The name offered is lexical only: containment is enforced by the root at the
// open itself, atomically, so no component can be swapped between a check and a
// read. O_NONBLOCK keeps a FIFO or device node from parking the turn inside
// open, and the descriptor answers the regular-file question. What it returns
// is already proven to be a regular file inside the root.
func openHandoffFile(dir, path string, index int) (io.ReadCloser, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, handoffInputError(index, imageErrorPathNotAllowed, handoffCauseRootUnresolved)
	}

	// The opened file outlives the directory handle that vouched for it.
	defer root.Close()

	// Two absolute paths always relate; a name that cannot be made relative is
	// offered as it stands and refused by the root like any other escape.
	name, _ := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))

	file, err := root.OpenFile(name, os.O_RDONLY|handoffOpenFlags, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, handoffInputError(index, imageErrorMissingFile, handoffCauseAbsent)
		}

		return nil, handoffInputError(index, imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	}

	// A descriptor that cannot be interrogated has not been proven regular, so
	// it is refused on the same terms as a directory, a FIFO or a device node.
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()

		return nil, handoffInputError(index, imageErrorPathNotAllowed, handoffCauseNotRegular)
	}

	return file, nil
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

// handoffDigestMatches compares the read bytes against the declared digest in
// constant time, so a caller cannot learn a byte-by-byte prefix of a file it
// only guessed at from how long the comparison took.
func handoffDigestMatches(decoded []byte, digest string) bool {
	sum := sha256.Sum256(decoded)
	declared, _ := hex.DecodeString(digest)

	return subtle.ConstantTimeCompare(sum[:], declared) == 1
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
