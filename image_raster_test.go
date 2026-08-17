package opencodeacp

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func pngChunks(chunks ...[]byte) []byte {
	data := []byte("\x89PNG\r\n\x1a\n")
	for _, chunk := range chunks {
		data = append(data, chunk...)
	}

	return data
}

func pngChunk(chunkType string, payload []byte) []byte {
	chunk := make([]byte, 0, 12+len(payload))
	chunk = binary.BigEndian.AppendUint32(chunk, uint32(len(payload)))
	chunk = append(chunk, chunkType...)
	chunk = append(chunk, payload...)
	chunk = append(chunk, 0, 0, 0, 0)

	return chunk
}

func pngIHDR(width, height uint32) []byte {
	payload := make([]byte, 13)
	binary.BigEndian.PutUint32(payload[0:4], width)
	binary.BigEndian.PutUint32(payload[4:8], height)

	return pngChunk("IHDR", payload)
}

func gifHeader(width, height uint16, packed byte) []byte {
	data := []byte("GIF89a")
	data = binary.LittleEndian.AppendUint16(data, width)
	data = binary.LittleEndian.AppendUint16(data, height)
	data = append(data, packed, 0, 0)

	return data
}

func gifImageDescriptor(localPacked byte) []byte {
	descriptor := []byte{0x2c, 0, 0, 0, 0, 1, 0, 1, 0, localPacked}
	if localPacked&0x80 != 0 {
		descriptor = append(descriptor, make([]byte, 3*(2<<(localPacked&0x07)))...)
	}

	// Minimum LZW code size, one data sub-block, terminator.
	descriptor = append(descriptor, 0x02, 0x01, 0x00, 0x00)

	return descriptor
}

func webpFile(chunk string, payload []byte) []byte {
	data := []byte("RIFF\x00\x00\x00\x00WEBP")
	data = append(data, chunk...)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(payload)))
	data = append(data, payload...)

	return data
}

func TestSniffRasterMIME(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		mime string
		ok   bool
	}{
		{name: "png", data: fixtureImage(t, "valid.png"), mime: mimePNG, ok: true},
		{name: "jpeg", data: fixtureImage(t, "valid.jpg"), mime: mimeJPEG, ok: true},
		{name: "gif", data: fixtureImage(t, "valid.gif"), mime: mimeGIF, ok: true},
		{name: "webp", data: fixtureImage(t, "valid.webp"), mime: mimeWebP, ok: true},
		{name: "bmp", data: append([]byte("BM"), make([]byte, 30)...), mime: mimeBMP, ok: true},
		{name: "short bmp", data: []byte("BM123")},
		{name: "ico", data: []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x00}, mime: mimeICO, ok: true},
		{name: "tiff little endian", data: []byte("II*\x00rest"), mime: mimeTIFF, ok: true},
		{name: "tiff big endian", data: []byte("MM\x00*rest"), mime: mimeTIFF, ok: true},
		{name: "riff without webp", data: []byte("RIFF\x00\x00\x00\x00WAVE")},
		{name: "garbage", data: []byte("not an image at all")},
		{name: "empty", data: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mime, ok := sniffRasterMIME(tt.data)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.mime, mime)
		})
	}
}

func TestInspectRasterFixtures(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		mime     string
		ok       bool
		animated bool
	}{
		{name: "valid png", fixture: "valid.png", mime: mimePNG, ok: true},
		{name: "valid jpeg", fixture: "valid.jpg", mime: mimeJPEG, ok: true},
		{name: "valid gif", fixture: "valid.gif", mime: mimeGIF, ok: true},
		{name: "valid webp", fixture: "valid.webp", mime: mimeWebP, ok: true},
		{name: "animated gif", fixture: "animated.gif", mime: mimeGIF, ok: true, animated: true},
		{name: "animated webp", fixture: "animated.webp", mime: mimeWebP, ok: true, animated: true},
		{name: "two frame apng", fixture: "animated-apng.png", mime: mimePNG, ok: true, animated: true},
		{name: "single frame acTL png", fixture: "single-frame-actl.png", mime: mimePNG, ok: true, animated: true},
		{name: "jpeg bytes named png", fixture: "mismatch.png", mime: mimeJPEG, ok: true},
		{name: "truncated png", fixture: "truncated.png", mime: mimePNG},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := inspectRaster(fixtureImage(t, tt.fixture))
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.mime, info.MIME)
			require.Equal(t, tt.animated, info.Animated)

			if tt.ok {
				require.Positive(t, info.Width)
				require.Positive(t, info.Height)
			}
		})
	}
}

func TestInspectRasterStructuralEdges(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		ok       bool
		animated bool
	}{
		{name: "unsniffable", data: []byte("garbage bytes")},
		{name: "bmp has no walker", data: append([]byte("BM"), make([]byte, 30)...), ok: true},
		{name: "png missing ihdr", data: pngChunks(pngChunk("IDAT", make([]byte, 13)))},
		{name: "png zero width", data: pngChunks(pngIHDR(0, 4))},
		{name: "png ihdr then end of data", data: pngChunks(pngIHDR(2, 2)), ok: true},
		{name: "png iend without idat", data: pngChunks(pngIHDR(2, 2), pngChunk("IEND", nil)), ok: true},
		{name: "png actl after header", data: pngChunks(pngIHDR(2, 2), pngChunk("acTL", make([]byte, 8))), ok: true, animated: true},
		{
			name: "jpeg fill bytes before frame",
			data: append([]byte{0xff, 0xd8, 0xff, 0xff, 0xff},
				[]byte{0xc0, 0x00, 0x11, 0x08, 0x00, 0x02, 0x00, 0x03}...),
			ok: true,
		},
		{
			name: "jpeg standalone marker before frame",
			data: append([]byte{0xff, 0xd8, 0xff, 0x01, 0xff, 0xd0},
				[]byte{0xff, 0xc0, 0x00, 0x11, 0x08, 0x00, 0x02, 0x00, 0x03}...),
			ok: true,
		},
		{
			name: "jpeg skips non frame segment",
			data: append([]byte{0xff, 0xd8, 0xff, 0xc4, 0x00, 0x03, 0xaa},
				[]byte{0xff, 0xcf, 0x00, 0x11, 0x08, 0x00, 0x02, 0x00, 0x03}...),
			ok: true,
		},
		{name: "jpeg lost marker alignment", data: []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x04, 0xaa, 0xbb, 0x00, 0x00, 0x00, 0x00}},
		{name: "jpeg segment length too short", data: []byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x01}},
		{name: "jpeg frame header truncated", data: []byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x11, 0x08}},
		{name: "jpeg zero dimensions", data: []byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x11, 0x08, 0x00, 0x00, 0x00, 0x00}},
		{name: "jpeg scan before frame", data: []byte{0xff, 0xd8, 0xff, 0xda, 0x00, 0x02, 0x00, 0x00}},
		{name: "jpeg runs out of segments", data: []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x04, 0x00, 0x00}},
		{name: "gif header too short", data: []byte("GIF89a\x01\x00")},
		{name: "gif zero width", data: gifHeader(0, 1, 0x00)},
		{name: "gif trailer without image", data: append(gifHeader(1, 1, 0x00), 0x3b)},
		{name: "gif unknown block byte", data: append(gifHeader(1, 1, 0x00), 0x7f)},
		{name: "gif missing trailer", data: gifHeader(1, 1, 0x00)},
		{
			name: "gif single image with tables",
			data: append(append(gifHeader(1, 1, 0x80), make([]byte, 6)...), append(gifImageDescriptor(0x80), 0x3b)...),
			ok:   true,
		},
		{
			name: "gif second descriptor is animation",
			data: append(append(gifHeader(1, 1, 0x00), gifImageDescriptor(0x00)...), gifImageDescriptor(0x00)...),
			ok:   true, animated: true,
		},
		{name: "gif truncated image descriptor", data: append(gifHeader(1, 1, 0x00), 0x2c, 0x00)},
		{name: "gif truncated sub blocks", data: append(gifHeader(1, 1, 0x00), 0x2c, 0, 0, 0, 0, 1, 0, 1, 0, 0x00, 0x02, 0x05)},
		{name: "gif truncated extension", data: append(gifHeader(1, 1, 0x00), 0x21, 0xfe, 0x10)},
		{
			name: "gif extension then image",
			data: append(append(gifHeader(1, 1, 0x00), 0x21, 0xfe, 0x01, 0x41, 0x00), append(gifImageDescriptor(0x00), 0x3b)...),
			ok:   true,
		},
		{name: "webp header only", data: []byte("RIFF\x00\x00\x00\x00WEBP")},
		{name: "webp vp8x still", data: webpFile("VP8X", []byte{0x00, 0, 0, 0, 1, 0, 0, 1, 0, 0}), ok: true},
		{name: "webp vp8x animated", data: webpFile("VP8X", []byte{0x02, 0, 0, 0, 1, 0, 0, 1, 0, 0}), ok: true, animated: true},
		{name: "webp vp8x truncated", data: webpFile("VP8X", []byte{0x02, 0, 0})},
		{name: "webp vp8 lossy", data: webpFile("VP8 ", []byte{0x00, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0x02, 0x00, 0x02, 0x00}), ok: true},
		{name: "webp vp8 bad start code", data: webpFile("VP8 ", []byte{0x00, 0x00, 0x00, 0xff, 0x01, 0x2a, 0x02, 0x00, 0x02, 0x00})},
		{name: "webp vp8 zero width", data: webpFile("VP8 ", []byte{0x00, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0x00, 0x00, 0x02, 0x00})},
		{name: "webp vp8l lossless", data: webpFile("VP8L", []byte{0x2f, 0x01, 0x40, 0x00, 0x00}), ok: true},
		{name: "webp vp8l bad signature", data: webpFile("VP8L", []byte{0x30, 0x01, 0x40, 0x00, 0x00})},
		{name: "webp vp8l truncated", data: webpFile("VP8L", []byte{0x2f, 0x01})},
		{name: "webp unknown first chunk", data: webpFile("ICCP", make([]byte, 12))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := inspectRaster(tt.data)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.animated, info.Animated)
		})
	}
}
