package vision

import (
	"context"
	"image"
	"math"
)

const lanczosSupport = 3.0

func PlanGeometry(srcW, srcH int, maxPixels int64, maxDim int) (dstW, dstH int, resized bool, err error) {
	if srcW <= 0 || srcH <= 0 {
		return 0, 0, false, Errorf(ErrorCodeInvalidArgument, "source dimensions are invalid")
	}
	if maxPixels <= 0 || maxDim <= 0 {
		return 0, 0, false, Errorf(ErrorCodeInvalidArgument, "transform bounds must be positive")
	}
	pixels := int64(srcW) * int64(srcH)
	if srcW <= maxDim && srcH <= maxDim && pixels <= maxPixels {
		return srcW, srcH, false, nil
	}
	scale := 1.0
	if srcW > maxDim {
		scale = math.Min(scale, float64(maxDim)/float64(srcW))
	}
	if srcH > maxDim {
		scale = math.Min(scale, float64(maxDim)/float64(srcH))
	}
	if pixels > maxPixels {
		scale = math.Min(scale, math.Sqrt(float64(maxPixels)/float64(pixels)))
	}
	dstW = int(math.Floor(float64(srcW) * scale))
	dstH = int(math.Floor(float64(srcH) * scale))
	dstW = min(max(dstW, 1), srcW)
	dstH = min(max(dstH, 1), srcH)
	resized = true
	return dstW, dstH, resized, nil
}

func TransformSatisfies(width, height int, maxPixels int64, maxDim int, version, wantVersion string) bool {
	if version == "" || version != wantVersion {
		return false
	}
	if width <= 0 || height <= 0 {
		return false
	}
	if width > maxDim || height > maxDim {
		return false
	}
	return int64(width)*int64(height) <= maxPixels
}

func ResizeLanczos3(ctx context.Context, src image.Image, dstW, dstH int, maxMemoryBytes int64) (image.Image, error) {
	if ctx == nil {
		return nil, errNilArgument("context")
	}
	if src == nil {
		return nil, errNilArgument("source image")
	}
	if dstW <= 0 || dstH <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "target dimensions are invalid")
	}
	if maxMemoryBytes <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "transform memory bound must be positive")
	}
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, Errorf(ErrorCodeVisionDecodeFailed, "source image is empty")
	}
	if exceedsTransformMem(srcW, srcH, 32, maxMemoryBytes) ||
		exceedsTransformMem(dstW, srcH, 32, maxMemoryBytes) ||
		exceedsTransformMem(dstW, dstH, 32, maxMemoryBytes) ||
		exceedsTransformMem(dstW, dstH, 4, maxMemoryBytes) {
		return nil, Errorf(ErrorCodeVisionLimitExceeded, "resized image exceeds the transform memory bound")
	}
	if dstW > srcW || dstH > srcH {
		return nil, Errorf(ErrorCodeInvalidArgument, "transform must not upscale")
	}
	if err := ctx.Err(); err != nil {
		return nil, Errorf(ErrorCodeVisionDecodeFailed, "transform was cancelled")
	}
	flat := imageToGray16Grid(src, bounds)
	tmp := resampleHorizontal(ctx, flat, srcW, srcH, dstW)
	if tmp == nil {
		return nil, Errorf(ErrorCodeVisionDecodeFailed, "transform was cancelled")
	}
	out := resampleVertical(ctx, tmp, dstW, srcH, dstH)
	if out == nil {
		return nil, Errorf(ErrorCodeVisionDecodeFailed, "transform was cancelled")
	}
	return gridToNRGBA(out, dstW, dstH), nil
}

type floatGrid struct {
	pixels []float64
	stride int
}

func imageToGray16Grid(src image.Image, bounds image.Rectangle) *floatGrid {
	width, height := bounds.Dx(), bounds.Dy()
	pixels := make([]float64, 4*width*height)
	for y := range height {
		for x := range width {
			r, g, b, a := src.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			base := (y*width + x) * 4
			pixels[base] = float64(r)
			pixels[base+1] = float64(g)
			pixels[base+2] = float64(b)
			pixels[base+3] = float64(a)
		}
	}
	return &floatGrid{pixels: pixels, stride: width}
}

func lanczos3(x float64) float64 {
	if x == 0 {
		return 1
	}
	if x < -lanczosSupport || x > lanczosSupport {
		return 0
	}
	pix := math.Pi * x
	return (math.Sin(pix) / pix) * (math.Sin(pix/lanczosSupport) / (pix / lanczosSupport))
}

type kernelTap struct {
	index  int
	weight float64
}

func lanczosTaps(center float64, srcLen, dstLen int) []kernelTap {
	scale := float64(dstLen) / float64(srcLen)
	radius := lanczosSupport
	if scale < 1 {
		radius = lanczosSupport / scale
	}
	left := int(math.Ceil(center - radius))
	right := int(math.Floor(center + radius))
	var taps []kernelTap
	var sum float64
	for i := left; i <= right; i++ {
		clamped := min(max(i, 0), srcLen-1)
		weight := lanczos3((center - float64(i)) * scale)
		if weight == 0 {
			continue
		}
		taps = append(taps, kernelTap{index: clamped, weight: weight})
		sum += weight
	}
	if sum == 0 {
		return []kernelTap{{index: min(max(int(center), 0), srcLen-1), weight: 1}}
	}
	for i := range taps {
		taps[i].weight /= sum
	}
	return taps
}

func resampleHorizontal(ctx context.Context, src *floatGrid, srcW, srcH, dstW int) *floatGrid {
	dst := &floatGrid{pixels: make([]float64, 4*dstW*srcH), stride: dstW}
	scale := float64(srcW) / float64(dstW)
	for d := range dstW {
		if d%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil
			}
		}
		center := (float64(d)+0.5)*scale - 0.5
		taps := lanczosTaps(center, srcW, dstW)
		for line := range srcH {
			for c := range 4 {
				var acc float64
				for _, tap := range taps {
					acc += src.pixels[(line*src.stride+tap.index)*4+c] * tap.weight
				}
				dst.pixels[(line*dstW+d)*4+c] = acc
			}
		}
	}
	return dst
}

func resampleVertical(ctx context.Context, src *floatGrid, width, srcH, dstH int) *floatGrid {
	dst := &floatGrid{pixels: make([]float64, 4*width*dstH), stride: width}
	scale := float64(srcH) / float64(dstH)
	for d := range dstH {
		if d%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil
			}
		}
		center := (float64(d)+0.5)*scale - 0.5
		taps := lanczosTaps(center, srcH, dstH)
		for col := range width {
			for c := range 4 {
				var acc float64
				for _, tap := range taps {
					acc += src.pixels[(tap.index*src.stride+col)*4+c] * tap.weight
				}
				dst.pixels[(d*width+col)*4+c] = acc
			}
		}
	}
	return dst
}

func gridToNRGBA(grid *floatGrid, width, height int) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			base := (y*width + x) * 4
			alpha := clamp01(grid.pixels[base+3] / 0xffff)
			var red, green, blue float64
			if alpha*255 < 0.5 {
				alpha = 0
			} else {
				red = clamp01(grid.pixels[base]/0xffff/alpha)
				green = clamp01(grid.pixels[base+1]/0xffff/alpha)
				blue = clamp01(grid.pixels[base+2]/0xffff/alpha)
			}
			out.Pix[(y*out.Stride)+x*4] = uint8(math.Round(red * 255))
			out.Pix[(y*out.Stride)+x*4+1] = uint8(math.Round(green * 255))
			out.Pix[(y*out.Stride)+x*4+2] = uint8(math.Round(blue * 255))
			out.Pix[(y*out.Stride)+x*4+3] = uint8(math.Round(alpha * 255))
		}
	}
	return out
}

func exceedsTransformMem(w, h int, perPixel, maxBytes int64) bool {
	if w <= 0 || h <= 0 || perPixel <= 0 {
		return true
	}
	return int64(h) > maxBytes/perPixel/int64(w)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
