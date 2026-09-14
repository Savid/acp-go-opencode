package opencodeacp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func cloneArtifacts(values map[string]imageArtifact) map[string]imageArtifact {
	result := make(map[string]imageArtifact, len(values))
	for k, v := range values {
		result[k] = v
	}

	return result
}
func (s *session) imageBytes(file opencode.NativeAttachment) ([]byte, string, *image.OutputError) {
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

	if file.ID != "" {
		s.mu.Lock()
		if s.artifacts == nil {
			s.artifacts = map[string]imageArtifact{}
		}

		s.artifacts[file.ID] = imageArtifact{Data: base64.StdEncoding.EncodeToString(data), MIME: mime, Reference: imageReference(file.URL)}
		s.mu.Unlock()
	}

	return data, mime, nil
}
func (s *session) outputFile(file opencode.NativeAttachment, used *int64) []acp.ContentBlock {
	if parsed, err := url.Parse(file.URL); err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") {
		return []acp.ContentBlock{{ResourceLink: &acp.ContentBlockResourceLink{Uri: file.URL, Name: file.Filename}}}
	}

	if !image.IsImageMIME(file.Mime) {
		return nil
	}

	data, mime, refusal := s.imageBytes(file)
	if refusal == nil && used != nil {
		*used += int64(len(data))
		if *used > s.agent.options.ImageLimits.core().EffectiveOutputPerToolCall() {
			refusal = &image.OutputError{Reason: image.ReasonTooLarge}
		}
	}

	if refusal != nil {
		s.mu.Lock()
		if s.artifacts == nil {
			s.artifacts = map[string]imageArtifact{}
		}

		s.artifacts[file.ID] = imageArtifact{Refusal: refusal.Reason, Reference: imageReference(file.URL)}
		s.mu.Unlock()

		guidance, _ := refusal.Guidance()

		return []acp.ContentBlock{acp.TextBlock(guidance)}
	}

	return []acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString(data), mime)}
}
func (s *session) captureImages(rows [][]byte) {
	messages := nativeMessages(rows, string(s.id))
	for index := range messages {
		message := &messages[index]
		for index := range message.Parts {
			part := &message.Parts[index]
			if part.Type == partFile && image.IsImageMIME(part.Mime) {
				_ = s.outputFile(opencode.NativeAttachment{ID: part.ID, Mime: part.Mime, URL: part.URL}, nil)
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
					if image.IsImageMIME(file.Mime) {
						_ = s.outputFile(file, &used)
					}
				}
			}
		}
	}
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

	messages := nativeMessages(rows, record.SessionID)
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
