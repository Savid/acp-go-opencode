package opencodeacp

import (
	"bytes"
	"encoding/binary"
)

const (
	mimePNG  = "image/png"
	mimeJPEG = "image/jpeg"
	mimeGIF  = "image/gif"
	mimeWebP = "image/webp"
	mimeBMP  = "image/bmp"
	mimeICO  = "image/x-icon"
	mimeTIFF = "image/tiff"
)

// rasterInfo is the result of a structural, decode-free image inspection:
// signature sniffing, header dimensions, and container-level animation
// markers. No raster is ever decoded.
type rasterInfo struct {
	MIME     string
	Width    int
	Height   int
	Animated bool
}

// sniffRasterMIME reports the canonical MIME type for a recognized raster
// signature. It recognizes more formats than the input allowlist because
// output emission is not allowlisted and reports the truthful sniffed type.
func sniffRasterMIME(data []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return mimePNG, true
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return mimeJPEG, true
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return mimeGIF, true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return mimeWebP, true
	case bytes.HasPrefix(data, []byte("BM")) && len(data) >= 26:
		return mimeBMP, true
	case bytes.HasPrefix(data, []byte("\x00\x00\x01\x00")) && len(data) >= 6:
		return mimeICO, true
	case bytes.HasPrefix(data, []byte("II*\x00")) || bytes.HasPrefix(data, []byte("MM\x00*")):
		return mimeTIFF, true
	default:
		return "", false
	}
}

// inspectRaster sniffs the signature and walks the container structurally.
// The returned MIME is set whenever a signature matched, even when the rest
// of the walk fails, so callers can distinguish an unrecognized byte stream
// from a recognized container with an unreadable header.
func inspectRaster(data []byte) (rasterInfo, bool) {
	mime, ok := sniffRasterMIME(data)
	if !ok {
		return rasterInfo{}, false
	}

	info := rasterInfo{MIME: mime}

	switch mime {
	case mimePNG:
		return inspectPNG(data, info)
	case mimeJPEG:
		return inspectJPEG(data, info)
	case mimeGIF:
		return inspectGIF(data, info)
	case mimeWebP:
		return inspectWebP(data, info)
	default:
		return info, true
	}
}

// inspectPNG reads IHDR dimensions and walks the chunk list for an acTL
// chunk (APNG). The APNG specification constrains acTL to precede the first
// IDAT, so the walk ends there.
func inspectPNG(data []byte, info rasterInfo) (rasterInfo, bool) {
	const headerLen = 8

	if len(data) < headerLen+25 || !bytes.Equal(data[12:16], []byte("IHDR")) {
		return info, false
	}

	info.Width = int(binary.BigEndian.Uint32(data[16:20]))
	info.Height = int(binary.BigEndian.Uint32(data[20:24]))

	if info.Width <= 0 || info.Height <= 0 {
		return info, false
	}

	offset := headerLen
	for offset+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		chunkType := string(data[offset+4 : offset+8])

		switch chunkType {
		case "acTL":
			info.Animated = true

			return info, true
		case "IDAT", "IEND":
			return info, true
		}

		offset += 12 + length
	}

	return info, true
}

// inspectJPEG walks the marker segments to the first frame header for
// dimensions.
func inspectJPEG(data []byte, info rasterInfo) (rasterInfo, bool) {
	offset := 2
	for offset+4 <= len(data) {
		if data[offset] != 0xff {
			return info, false
		}

		marker := data[offset+1]
		if marker == 0xff {
			offset++

			continue
		}

		// Standalone markers carry no length payload.
		if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			offset += 2

			continue
		}

		length := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if length < 2 {
			return info, false
		}

		if isJPEGFrameMarker(marker) {
			if offset+9 > len(data) {
				return info, false
			}

			info.Height = int(binary.BigEndian.Uint16(data[offset+5 : offset+7]))
			info.Width = int(binary.BigEndian.Uint16(data[offset+7 : offset+9]))

			return info, info.Width > 0 && info.Height > 0
		}

		// Entropy-coded data follows the scan header; without decoding it
		// there is no frame header left to find.
		if marker == 0xda {
			return info, false
		}

		offset += 2 + length
	}

	return info, false
}

// isJPEGFrameMarker reports whether marker is a start-of-frame marker
// carrying dimensions (SOF0-SOF15 excluding DHT/JPG/DAC).
func isJPEGFrameMarker(marker byte) bool {
	if marker < 0xc0 || marker > 0xcf {
		return false
	}

	return marker != 0xc4 && marker != 0xc8 && marker != 0xcc
}

// inspectGIF reads the logical screen descriptor and counts image
// descriptors through the block stream; more than one means animation.
func inspectGIF(data []byte, info rasterInfo) (rasterInfo, bool) {
	const screenDescriptorEnd = 13

	if len(data) < screenDescriptorEnd {
		return info, false
	}

	info.Width = int(binary.LittleEndian.Uint16(data[6:8]))
	info.Height = int(binary.LittleEndian.Uint16(data[8:10]))

	if info.Width <= 0 || info.Height <= 0 {
		return info, false
	}

	offset := screenDescriptorEnd

	packed := data[10]
	if packed&0x80 != 0 {
		offset += 3 * (2 << (packed & 0x07))
	}

	images := 0

	for offset < len(data) {
		switch data[offset] {
		case 0x2c:
			images++
			if images > 1 {
				info.Animated = true

				return info, true
			}

			if offset+10 > len(data) {
				return info, false
			}

			localPacked := data[offset+9]
			offset += 10

			if localPacked&0x80 != 0 {
				offset += 3 * (2 << (localPacked & 0x07))
			}

			next, ok := skipGIFSubBlocks(data, offset+1)
			if !ok {
				return info, false
			}

			offset = next
		case 0x21:
			next, ok := skipGIFSubBlocks(data, offset+2)
			if !ok {
				return info, false
			}

			offset = next
		case 0x3b:
			return info, images > 0
		default:
			return info, false
		}
	}

	return info, false
}

// skipGIFSubBlocks advances past a GIF data sub-block chain starting at
// offset and returns the offset after its terminator.
func skipGIFSubBlocks(data []byte, offset int) (int, bool) {
	for {
		if offset >= len(data) {
			return 0, false
		}

		size := int(data[offset])
		if size == 0 {
			return offset + 1, true
		}

		offset += 1 + size
	}
}

// inspectWebP reads dimensions from the VP8X, VP8, or VP8L chunk and reports
// animation from the VP8X ANIM flag.
func inspectWebP(data []byte, info rasterInfo) (rasterInfo, bool) {
	const riffHeaderLen = 12

	if len(data) < riffHeaderLen+8 {
		return info, false
	}

	chunk := string(data[riffHeaderLen : riffHeaderLen+8][:4])
	payload := data[riffHeaderLen+8:]

	switch chunk {
	case "VP8X":
		if len(payload) < 10 {
			return info, false
		}

		info.Animated = payload[0]&0x02 != 0
		info.Width = 1 + int(uint32(payload[4])|uint32(payload[5])<<8|uint32(payload[6])<<16)
		info.Height = 1 + int(uint32(payload[7])|uint32(payload[8])<<8|uint32(payload[9])<<16)

		return info, true
	case "VP8 ":
		if len(payload) < 10 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
			return info, false
		}

		info.Width = int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
		info.Height = int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)

		return info, info.Width > 0 && info.Height > 0
	case "VP8L":
		if len(payload) < 5 || payload[0] != 0x2f {
			return info, false
		}

		bits := binary.LittleEndian.Uint32(payload[1:5])
		info.Width = 1 + int(bits&0x3fff)
		info.Height = 1 + int((bits>>14)&0x3fff)

		return info, true
	default:
		return info, false
	}
}
