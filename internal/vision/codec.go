package vision

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"
)

func decodeWebP(raw []byte) (image.Image, error) {
	img, err := webp.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return img, nil
}

func encodeNormalized(mime string, img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	switch mime {
	case MIMEPNG, MIMEgif, MIMEWebP:
		if err := png.Encode(&buf, img); err != nil {
			return nil, Errorf(ErrorCodeVisionDecodeFailed, "normalized encode did not complete")
		}
		return buf.Bytes(), nil
	case MIMEJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
			return nil, Errorf(ErrorCodeVisionDecodeFailed, "normalized encode did not complete")
		}
		return buf.Bytes(), nil
	default:
		return nil, Errorf(ErrorCodeVisionFormatUnsupported, "image format is not supported")
	}
}
