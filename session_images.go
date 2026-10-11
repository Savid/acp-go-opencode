package opencodeacp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// imageArtifact preserves admitted local output independently of the native path.
type imageArtifact struct {
	Data      string `json:"data"`
	MIME      string `json:"mime"`
	Refusal   string `json:"refusal,omitempty"`
	Reference string `json:"reference"`
}

// Artifacts a session keeps for prompt references are bounded by count and by
// retained bytes; the oldest are dropped first.
const (
	maxArtifacts     = 32
	maxArtifactBytes = 64 << 20
)

// keepArtifact records one artifact and evicts the oldest past the bounds.
func (s *session) keepArtifact(id string, artifact imageArtifact) {
	if s.artifacts == nil {
		s.artifacts = map[string]imageArtifact{}
	}

	s.artifactOrder = keepArtifact(s.artifacts, s.artifactOrder, id, artifact)
}

// keepArtifact records one artifact in artifacts, whose ids are in order
// oldest first, evicts the oldest past the bounds, and returns the order.
func keepArtifact(artifacts map[string]imageArtifact, order []string, id string, artifact imageArtifact) []string {
	if _, present := artifacts[id]; !present {
		order = append(order, id)
	}

	artifacts[id] = artifact

	for len(order) > maxArtifacts || artifactBytes(artifacts) > maxArtifactBytes {
		delete(artifacts, order[0])
		order = order[1:]
	}

	return order
}

func artifactBytes(artifacts map[string]imageArtifact) int {
	total := 0
	for _, artifact := range artifacts {
		total += len(artifact.Data)
	}

	return total
}

func cloneArtifacts(values map[string]imageArtifact) map[string]imageArtifact {
	result := make(map[string]imageArtifact, len(values))
	maps.Copy(result, values)

	return result
}

// readImage reads one image through the per-image output gate, from the
// session's captured artifact when it still names the same location.
func (s *session) readImage(file opencode.NativeAttachment) ([]byte, string, *image.OutputError) {
	limit := s.agent.options.ImageLimits.core().EffectiveOutputPerImage()
	s.mu.Lock()
	cached, ok := s.artifacts[file.ID]
	s.mu.Unlock()

	if ok && cached.Reference == imageReference(file.URL) {
		if cached.Refusal != "" {
			return nil, "", &image.OutputError{Reason: cached.Refusal}
		}

		data, mime, _, err := image.DecodeInline(cached.Data, limit)

		return data, mime, err
	}

	var (
		data    []byte
		mime    string
		refusal *image.OutputError
	)

	if strings.HasPrefix(file.URL, "data:") {
		prefix, payload, ok := strings.Cut(file.URL, ",")
		if !ok || !strings.HasSuffix(prefix, ";base64") {
			return nil, "", &image.OutputError{Reason: image.ReasonInvalidBase64}
		}

		data, mime, _, refusal = image.DecodeInline(payload, limit)
	} else {
		path := file.URL
		if parsed, err := url.Parse(path); err == nil && parsed.Scheme == partFile {
			if parsed.Host != "" && parsed.Host != "localhost" {
				return nil, "", &image.OutputError{Reason: image.ReasonPathNotAllowed}
			}

			path = parsed.Path
		}

		if !filepath.IsAbs(path) {
			return nil, "", &image.OutputError{Reason: image.ReasonPathNotAllowed}
		}

		roots := append([]string{s.cwd, os.TempDir()}, s.additionalDirectories...)
		if s.agent.options.ScratchDir != "" {
			roots = append(roots, s.agent.options.ScratchDir)
		}

		data, mime, refusal = image.ReadFile(path, roots, limit)
	}

	if refusal != nil {
		return nil, "", refusal
	}

	declared := strings.ToLower(strings.TrimSpace(file.Mime))
	switch declared {
	case image.MIMEPNG, image.MIMEJPEG, image.MIMEGIF, image.MIMEWebP, "image/bmp", "image/x-icon", "image/tiff":
		if declared != mime {
			return nil, "", &image.OutputError{Reason: image.ReasonMediaTypeMismatch}
		}
	}

	return data, mime, nil
}

// admittedImage is one image through the output gate: the bytes to show or
// the refusal, and for a storable file the artifact a replay needs and
// whether the session has yet to record it.
type admittedImage struct {
	data     []byte
	mime     string
	refusal  *image.OutputError
	artifact imageArtifact
	record   bool
}

// admitImage runs one image through the output gates, counting its bytes
// against used when a tool call shares that bound. It records nothing.
func (s *session) admitImage(file opencode.NativeAttachment, used *int64) admittedImage {
	data, mime, refusal := s.readImage(file)
	if refusal == nil && used != nil {
		*used += int64(len(data))
		if *used > s.agent.options.ImageLimits.core().EffectiveOutputPerToolCall() {
			refusal = &image.OutputError{Reason: image.ReasonTooLarge}
		}
	}

	admitted := admittedImage{data: data, mime: mime, refusal: refusal}
	if !storableArtifact(file) {
		return admitted
	}

	reference := imageReference(file.URL)

	s.mu.Lock()
	cached, held := s.artifacts[file.ID]
	s.mu.Unlock()

	reason := ""
	if refusal != nil {
		reason = refusal.Reason
	}

	switch {
	case held && cached.Reference == reference && cached.Refusal == reason:
		admitted.artifact = cached
	case refusal != nil:
		admitted.artifact, admitted.record = imageArtifact{Refusal: reason, Reference: reference}, true
	default:
		admitted.artifact, admitted.record = imageArtifact{Data: base64.StdEncoding.EncodeToString(data), MIME: mime, Reference: reference}, true
	}

	return admitted
}

func (s *session) outputFile(file opencode.NativeAttachment, used *int64) []acp.ContentBlock {
	if parsed, err := url.Parse(file.URL); err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") {
		return []acp.ContentBlock{{ResourceLink: &acp.ContentBlockResourceLink{Uri: file.URL, Name: file.Filename}}}
	}

	if !image.IsImageMIME(file.Mime) {
		return nil
	}

	admitted := s.admitImage(file, used)
	if admitted.record {
		s.mu.Lock()
		s.keepArtifact(file.ID, admitted.artifact)
		s.mu.Unlock()
	}

	if admitted.refusal != nil {
		guidance, _ := admitted.refusal.Guidance()

		return []acp.ContentBlock{acp.TextBlock(guidance)}
	}

	return []acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString(admitted.data), admitted.mime)}
}

// capturedImage is an artifact a commit records once it succeeds.
type capturedImage struct {
	id       string
	artifact imageArtifact
}

// captureImages admits every image the mirror cannot replay from its own rows
// through the output gate and returns the artifacts the session has yet to
// record, which a later replay reads. It reads the part updates among the
// rows a commit adds, since each carries its whole part, and records nothing,
// so a commit can call it again. Only a storable artifact is decoded: a data
// URL carries its bytes in the mirrored row, so rastering it here would cost
// a decode on every commit and store nothing.
func (s *session) captureImages(rows [][]byte) []capturedImage {
	var captured []capturedImage

	capture := func(file opencode.NativeAttachment, used *int64) {
		if !image.IsImageMIME(file.Mime) || !storableArtifact(file) {
			return
		}

		if admitted := s.admitImage(file, used); admitted.record {
			captured = append(captured, capturedImage{id: file.ID, artifact: admitted.artifact})
		}
	}

	for _, row := range rows {
		var event messageEvent
		if json.Unmarshal(row, &event) != nil || event.AggregateID != s.nativeID || event.Type != syncPartUpdated {
			continue
		}

		part := &event.Data.Part
		if part.Type == partFile {
			capture(opencode.NativeAttachment{ID: part.ID, Mime: part.Mime, URL: part.URL}, nil)
		}

		if part.Type == partTool {
			var state struct {
				Attachments []opencode.NativeAttachment `json:"attachments"`
			}
			if json.Unmarshal(part.State, &state) != nil {
				continue
			}

			used := int64(0)

			for index, file := range state.Attachments {
				file.ID = attachmentID(part.ID, index)
				capture(file, &used)
			}
		}
	}

	return captured
}

// storableArtifact reports whether an admitted image needs its own sidecar
// copy. A data URL already carries its bytes in the mirrored native row, so a
// second copy in the configuration record would be written on every commit.
func storableArtifact(file opencode.NativeAttachment) bool {
	return file.ID != "" && !strings.HasPrefix(file.URL, "data:")
}

func attachmentID(part string, index int) string { return part + "/" + strconv.Itoa(index) }

func imageReference(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:])
}

func validateStoredImages(rows [][]byte, record sessionRecord) error {
	check := func(file opencode.NativeAttachment) error {
		if !image.IsImageMIME(file.Mime) || strings.HasPrefix(file.URL, "data:") {
			return nil
		}

		if parsed, err := url.Parse(file.URL); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			return nil
		}

		cached, ok := record.Artifacts[file.ID]
		if !ok || cached.Reference != imageReference(file.URL) {
			return errors.New("stored local image artifact missing")
		}

		return nil
	}

	messages := nativeMessages(rows, record.NativeSessionID)
	for mi := range messages {
		message := &messages[mi]
		for pi := range message.Parts {
			part := &message.Parts[pi]
			if part.Type == partFile {
				if err := check(opencode.NativeAttachment{ID: part.ID, Mime: part.Mime, URL: part.URL}); err != nil {
					return err
				}
			}

			if part.Type == partTool {
				var state struct {
					Attachments []opencode.NativeAttachment `json:"attachments"`
				}
				if err := json.Unmarshal(part.State, &state); err != nil {
					return err
				}

				for index, file := range state.Attachments {
					file.ID = attachmentID(part.ID, index)
					if err := check(file); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}
