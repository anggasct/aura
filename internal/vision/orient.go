package vision

import (
	"bytes"
	"encoding/binary"
	"image"
)

func orientationOf(mime string, raw []byte) int {
	if mime != MIMEJPEG {
		return 1
	}
	orientation, ok := exifOrientation(raw)
	if !ok {
		return 1
	}
	if orientation < 1 || orientation > 8 {
		return 1
	}
	return orientation
}

func applyOrientation(img image.Image, orientation int, enabled bool) image.Image {
	if img == nil || !enabled || orientation == 1 {
		return img
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	swap := orientation >= 5
	outWidth, outHeight := width, height
	if swap {
		outWidth, outHeight = height, width
	}
	out := image.NewNRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := range height {
		for x := range width {
			at := img.At(bounds.Min.X+x, bounds.Min.Y+y)
			dx, dy := mapOrientation(x, y, width, height, orientation)
			out.Set(dx, dy, at)
		}
	}
	return out
}

func mapOrientation(x, y, width, height, orientation int) (dx, dy int) {
	switch orientation {
	case 2:
		return width - 1 - x, y
	case 3:
		return width - 1 - x, height - 1 - y
	case 4:
		return x, height - 1 - y
	case 5:
		return y, x
	case 6:
		return height - 1 - y, x
	case 7:
		return height - 1 - y, width - 1 - x
	case 8:
		return y, width - 1 - x
	default:
		return x, y
	}
}

func exifOrientation(raw []byte) (int, bool) {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 || raw[2] != 0xFF {
		return 1, false
	}
	pos := 2
	for pos+4 <= len(raw) {
		if raw[pos] != 0xFF {
			return 1, false
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
			return 1, false
		}
		length := int(binary.BigEndian.Uint16(raw[pos : pos+2]))
		if length < 2 || pos+length > len(raw) {
			return 1, false
		}
		if marker == 0xE1 {
			if orientation, ok := tiffOrientation(raw[pos+2 : pos+length]); ok {
				return orientation, true
			}
		}
		pos += length
	}
	return 1, false
}

func tiffOrientation(segment []byte) (int, bool) {
	if len(segment) < 14 || !bytes.Equal(segment[:6], []byte{'E', 'x', 'i', 'f', 0, 0}) {
		return 1, false
	}
	header := segment[6:]
	var order binary.ByteOrder
	switch {
	case bytes.Equal(header[:2], []byte{'I', 'I'}):
		order = binary.LittleEndian
	case bytes.Equal(header[:2], []byte{'M', 'M'}):
		order = binary.BigEndian
	default:
		return 1, false
	}
	if order.Uint16(header[2:4]) != 42 {
		return 1, false
	}
	offset := int(order.Uint32(header[4:8]))
	if offset < 0 || offset+2 > len(header) {
		return 1, false
	}
	count := int(order.Uint16(header[offset : offset+2]))
	cursor := offset + 2
	for range count {
		if cursor+12 > len(header) {
			return 1, false
		}
		tag := order.Uint16(header[cursor : cursor+2])
		typ := order.Uint16(header[cursor+2 : cursor+4])
		components := order.Uint32(header[cursor+4 : cursor+8])
		value := header[cursor+8 : cursor+12]
		cursor += 12
		if tag != 0x0112 || typ != 3 || components != 1 {
			continue
		}
		return int(order.Uint16(value[:2])), true
	}
	return 1, false
}
