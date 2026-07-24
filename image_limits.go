package opencodeacp

import (
	"errors"
	"fmt"
)

// defaultImageLimitBytes is the default decoded-byte bound for every
// ImageLimits field. It keeps a maximal single-image ACP frame inside the
// pinned SDK's 10 MiB connection scanner with headroom for JSON overhead and
// surrounding text.
const defaultImageLimitBytes int64 = 6 * 1024 * 1024

// imageFrameBoundBytes is the largest decoded image the pinned ACP SDK can
// carry in one session/update frame; a larger frame disconnects a pinned-SDK
// consumer before any handler runs. An emitted agent image travels alone in
// its chunk and a tool call's whole content array rides one frame, so both are
// clamped to this bound. It is a hard cap: a configured-larger output policy
// is reduced to it, and a disabled (zero) output policy still cannot exceed it.
const imageFrameBoundBytes int64 = 7864155

// effectiveOutputLimit clamps a configured output policy limit to the hard
// transport frame bound. Zero (policy disabled) still yields the frame bound.
func effectiveOutputLimit(configured int64) int64 {
	if configured <= 0 || configured > imageFrameBoundBytes {
		return imageFrameBoundBytes
	}

	return configured
}

// ImageLimits bounds decoded image bytes crossing the ACP boundary. Input
// fields bound prompt images before a native turn starts; output fields bound
// emitted image content. A zero field disables that adapter policy limit; it
// never bypasses hard native framing, provider, or host request limits.
// Negative fields are rejected when the agent is constructed.
type ImageLimits struct {
	MaxInputBytesPerImage     int64
	MaxInputBytesPerPrompt    int64
	MaxOutputBytesPerImage    int64
	MaxOutputBytesPerToolCall int64
}

func defaultImageLimits() ImageLimits {
	return ImageLimits{
		MaxInputBytesPerImage:     defaultImageLimitBytes,
		MaxInputBytesPerPrompt:    defaultImageLimitBytes,
		MaxOutputBytesPerImage:    defaultImageLimitBytes,
		MaxOutputBytesPerToolCall: defaultImageLimitBytes,
	}
}

// WithImageLimits replaces every image byte limit. The supplied struct owns
// all four fields: an unset field is zero and disables that policy limit.
func WithImageLimits(limits ImageLimits) Option {
	return func(options *Options) {
		options.ImageLimits = limits
	}
}

func validateImageLimits(limits ImageLimits) error {
	fields := []struct {
		name  string
		value int64
	}{
		{"MaxInputBytesPerImage", limits.MaxInputBytesPerImage},
		{"MaxInputBytesPerPrompt", limits.MaxInputBytesPerPrompt},
		{"MaxOutputBytesPerImage", limits.MaxOutputBytesPerImage},
		{"MaxOutputBytesPerToolCall", limits.MaxOutputBytesPerToolCall},
	}

	var err error

	for _, field := range fields {
		if field.value < 0 {
			err = errors.Join(err, fmt.Errorf("image limit %s cannot be negative", field.name))
		}
	}

	return err
}

func (s *session) imageLimits() ImageLimits {
	if s.agent == nil {
		return defaultImageLimits()
	}

	return s.agent.options.ImageLimits
}
