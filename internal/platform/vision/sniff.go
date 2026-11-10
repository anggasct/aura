package vision

import (
	"bytes"
	"encoding/binary"
	"strings"
)

const (
	MIMEPNG  = "image/png"
	MIMEJPEG = "image/jpeg"
	MIMEWebP = "image/webp"
	MIMEgif  = "image/gif"
)

const sniffPrefixLen = 32

type headerGeometry struct {
	width    int
	height   int
	animated bool
	frames   int
	color    string
}

func sniffFormat(prefix []byte) (mime string, ok bool) {
	if len(prefix) >= 8 && bytes.Equal(prefix[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return MIMEPNG, true
	}
	if len(prefix) >= 3 && prefix[0] == 0xFF && prefix[1] == 0xD8 && prefix[2] == 0xFF {
		return MIMEJPEG, true
	}
	if len(prefix) >= 12 && bytes.Equal(prefix[:4], []byte("RIFF")) && bytes.Equal(prefix[8:12], []byte("WEBP")) {
		return MIMEWebP, true
	}
	if len(prefix) >= 6 && (bytes.Equal(prefix[:6], []byte("GIF87a")) || bytes.Equal(prefix[:6], []byte("GIF89a"))) {
		return MIMEgif, true
	}
	return "", false
}

func sniffDimensions(mime string, raw []byte) (headerGeometry, error) {
	switch mime {
	case MIMEPNG:
		return pngDimensions(raw)
	case MIMEJPEG:
		return jpegDimensions(raw)
	case MIMEgif:
		return gifDimensions(raw)
	case MIMEWebP:
		return webpDimensions(raw)
	default:
		return headerGeometry{}, Errorf(ErrorCodeVisionFormatUnsupported, "format is not supported")
	}
}

func pngDimensions(raw []byte) (headerGeometry, error) {
	if len(raw) < 33 {
		return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "png header is truncated")
	}
	if !bytes.Equal(raw[12:16], []byte("IHDR")) {
		return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "png ihdr chunk is missing")
	}
	width := int(binary.BigEndian.Uint32(raw[16:20]))
	height := int(binary.BigEndian.Uint32(raw[20:24]))
	if width <= 0 || height <= 0 {
		return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "png dimensions are invalid")
	}
	bitDepth := raw[24]
	colorType := raw[25]
	var color string
	switch colorType {
	case 0:
		color = "grayscale"
	case 2:
		color = "rgb"
	case 3:
		color = "palette"
	case 4:
		color = "grayscale-alpha"
	case 6:
		color = "rgba"
	default:
		return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "png color type is not supported")
	}
	if bitDepth != 1 && bitDepth != 2 && bitDepth != 4 && bitDepth != 8 && bitDepth != 16 {
		return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "png bit depth is not supported")
	}
	animated := bytes.Contains(raw, []byte("acTL"))
	return headerGeometry{width: width, height: height, animated: animated, frames: 1, color: color}, nil
}

func jpegDimensions(raw []byte) (headerGeometry, error) {
	pos := 2
	for pos+4 <= len(raw) {
		if raw[pos] != 0xFF {
			return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "jpeg segment is malformed")
		}
		marker := raw[pos+1]
		pos += 2
		if marker == 0xD8 || marker == 0xD9 {
			continue
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if pos+2 > len(raw) {
			return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "jpeg segment is truncated")
		}
		length := int(binary.BigEndian.Uint16(raw[pos : pos+2]))
		if length < 2 {
			return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "jpeg segment length is invalid")
		}
		if marker == 0xC0 || marker == 0xC1 || marker == 0xC2 {
			if pos+7 > len(raw) {
				return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "jpeg frame header is truncated")
			}
			height := int(binary.BigEndian.Uint16(raw[pos+3 : pos+5]))
			width := int(binary.BigEndian.Uint16(raw[pos+5 : pos+7]))
			if width <= 0 || height <= 0 {
				return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "jpeg dimensions are invalid")
			}
			return headerGeometry{width: width, height: height, frames: 1, color: "ycbcr"}, nil
		}
		pos += length
	}
	return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "jpeg frame header is missing")
}

func gifDimensions(raw []byte) (headerGeometry, error) {
	if len(raw) < 10 {
		return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "gif header is truncated")
	}
	width := int(binary.LittleEndian.Uint16(raw[6:8]))
	height := int(binary.LittleEndian.Uint16(raw[8:10]))
	if width <= 0 || height <= 0 {
		return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "gif dimensions are invalid")
	}
	count := 0
	for i := range len(raw) - 1 {
		if raw[i] == 0x21 && raw[i+1] == 0xF9 {
			count++
		}
	}
	return headerGeometry{width: width, height: height, animated: count > 1, frames: 1, color: "palette"}, nil
}

func webpDimensions(raw []byte) (headerGeometry, error) {
	if len(raw) < 30 {
		return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "webp header is truncated")
	}
	if bytes.Equal(raw[12:16], []byte("VP8X")) {
		flags := raw[16+4]
		animated := flags&0x02 != 0
		width := int(uint32(raw[24]) | uint32(raw[25])<<8 | uint32(raw[26])<<16)
		height := int(uint32(raw[27]) | uint32(raw[28])<<8 | uint32(raw[29])<<16)
		width++
		height++
		if width <= 0 || height <= 0 {
			return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "webp dimensions are invalid")
		}
		return headerGeometry{width: width, height: height, animated: animated, frames: 1, color: "rgb"}, nil
	}
	if bytes.Equal(raw[12:16], []byte("VP8 ")) {
		if len(raw) < 30 {
			return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "webp frame is truncated")
		}
		width := int(binary.LittleEndian.Uint16(raw[26:28])) & 0x3FFF
		height := int(binary.LittleEndian.Uint16(raw[28:30])) & 0x3FFF
		if width <= 0 || height <= 0 {
			return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "webp dimensions are invalid")
		}
		return headerGeometry{width: width, height: height, frames: 1, color: "ycbcr"}, nil
	}
	if bytes.Equal(raw[12:16], []byte("VP8L")) {
		if len(raw) < 25 {
			return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "webp frame is truncated")
		}
		packed := binary.LittleEndian.Uint32(raw[21:25])
		width := int(packed&0x3FFF) + 1
		height := int((packed>>14)&0x3FFF) + 1
		if width <= 0 || height <= 0 {
			return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "webp dimensions are invalid")
		}
		return headerGeometry{width: width, height: height, frames: 1, color: "rgba"}, nil
	}
	return headerGeometry{}, Errorf(ErrorCodeVisionDecodeFailed, "webp chunk is not supported")
}

func normalizeDeclaredMIME(declared string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(declared, ";", 2)[0]))
}
