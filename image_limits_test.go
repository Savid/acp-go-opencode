package opencodeacp

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageLimitsDefaultsAndOption(t *testing.T) {
	defaults := defaultImageLimits()
	require.Equal(t, int64(6291456), defaults.MaxInputBytesPerImage)
	require.Equal(t, int64(6291456), defaults.MaxInputBytesPerPrompt)
	require.Equal(t, int64(6291456), defaults.MaxOutputBytesPerImage)
	require.Equal(t, int64(6291456), defaults.MaxOutputBytesPerToolCall)

	var options Options

	WithImageLimits(ImageLimits{MaxInputBytesPerImage: 1})(&options)
	require.Equal(t, int64(1), options.ImageLimits.MaxInputBytesPerImage)

	require.NoError(t, validateImageLimits(defaults))

	err := validateImageLimits(ImageLimits{
		MaxInputBytesPerImage:     -1,
		MaxInputBytesPerPrompt:    -1,
		MaxOutputBytesPerImage:    -1,
		MaxOutputBytesPerToolCall: -1,
	})
	require.ErrorContains(t, err, "MaxInputBytesPerImage")
	require.ErrorContains(t, err, "MaxOutputBytesPerToolCall")
}

func TestImageLimitsNilAgent(t *testing.T) {
	sess := testSession(t, NewAgent(), newFakeOpenCodeClient())
	sess.stopPump()
	sess.agent = nil
	require.Equal(t, defaultImageLimits(), sess.imageLimits())
}

func TestEffectiveOutputLimitClampsToFrameBound(t *testing.T) {
	// Zero disables the policy limit but never the hard transport frame cap.
	require.Equal(t, imageFrameBoundBytes, effectiveOutputLimit(0))
	// A configured value larger than the frame bound is reduced to it.
	require.Equal(t, imageFrameBoundBytes, effectiveOutputLimit(imageFrameBoundBytes+1))
	// A configured value under the frame bound is honored as-is.
	require.Equal(t, int64(1024), effectiveOutputLimit(1024))
}

func TestRegisterImageArtifactInitializesMaps(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	sess := testSession(t, NewAgent(), newFakeOpenCodeClient())
	sess.imageArtifacts = nil
	sess.imageArtifactIdentities = nil

	record := imageArtifactRecord{
		Version: imageArtifactRecordVersion, Fingerprint: imageFingerprint(png),
		Mime: mimePNG, Data: base64.StdEncoding.EncodeToString(png),
	}
	require.NoError(t, sess.registerImageArtifact(context.Background(), "id-1", record))

	_, ok := sess.imageArtifactByIdentity("id-1")
	require.True(t, ok)
}
