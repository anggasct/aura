package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"

	"golang.org/x/image/webp"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/vision"
)

const (
	visionArtifactScheme   = "artifact://"
	visionEnvelopeKey      = "image_ref"
	visionEnvelopeKind     = "image_ref.v1"
	maxVisionEnvelopeBytes = 1 << 20
	maxVisionRefIDLength   = 128
)

var (
	errNoGifFrames            = errors.New("model: gif has no frames")
	errUnsupportedVisionMedia = errors.New("model: unsupported image media type")
)

type VisionImage struct {
	RefID            string
	Digest           string
	MIME             string
	Width            int
	Height           int
	EncodedBytes     int64
	TransformVersion string
	Data             []byte
}

type visionEnvelope struct {
	Kind             string `json:"kind"`
	ArtifactID       string `json:"artifact_id"`
	DerivedDigest    string `json:"derived_digest"`
	MIME             string `json:"mime"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	EncodedBytes     int64  `json:"encoded_bytes"`
	TransformVersion string `json:"transform_version"`
}

func isVisionRefID(s string) bool {
	if s == "" || len(s) > maxVisionRefIDLength {
		return false
	}
	for i := range s {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func isSupportedVisionMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func parseVisionEnvelope(raw string) (visionEnvelope, error) {
	var env visionEnvelope
	if raw == "" || len(raw) > maxVisionEnvelopeBytes {
		return visionEnvelope{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference envelope is out of bounds")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&env); err != nil {
		return visionEnvelope{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference envelope is not decodable")
	}
	if env.Kind != visionEnvelopeKind {
		return visionEnvelope{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference kind is not supported")
	}
	if !isVisionRefID(env.ArtifactID) {
		return visionEnvelope{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference artifact id is invalid")
	}
	if !strings.HasPrefix(env.DerivedDigest, "sha256:") {
		return visionEnvelope{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference digest is invalid")
	}
	if !isSupportedVisionMIME(env.MIME) {
		return visionEnvelope{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference media type is not supported")
	}
	if env.Width <= 0 || env.Height <= 0 || env.EncodedBytes <= 0 {
		return visionEnvelope{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference dimensions are invalid")
	}
	if strings.TrimSpace(env.TransformVersion) == "" {
		return visionEnvelope{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference transform version is missing")
	}
	return env, nil
}

func collectVisionImages(contents []*genai.Content) ([]VisionImage, error) {
	var images []VisionImage
	for _, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			switch {
			case part.FileData != nil:
				img, err := visionImageFromFileData(part)
				if err != nil {
					return nil, err
				}
				images = append(images, img)
			case part.InlineData != nil && strings.HasPrefix(strings.ToLower(part.InlineData.MIMEType), "image/"):
				mime := strings.ToLower(part.InlineData.MIMEType)
				if !isSupportedVisionMIME(mime) {
					return nil, newError(ErrorCodeProtocolInvalid, "", "", "inline image media type is not supported")
				}
				images = append(images, VisionImage{MIME: mime, EncodedBytes: int64(len(part.InlineData.Data)), Data: part.InlineData.Data})
			}
		}
	}
	return images, nil
}

func visionImageFromFileData(part *genai.Part) (VisionImage, error) {
	uri := part.FileData.FileURI
	if !strings.HasPrefix(uri, visionArtifactScheme) {
		return VisionImage{}, newError(ErrorCodeProtocolInvalid, "", "", "file content reference is not supported")
	}
	refID := strings.TrimPrefix(uri, visionArtifactScheme)
	if !isVisionRefID(refID) {
		return VisionImage{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference artifact id is invalid")
	}
	raw, ok := part.PartMetadata[visionEnvelopeKey].(string)
	if !ok {
		return VisionImage{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference envelope is missing")
	}
	env, err := parseVisionEnvelope(raw)
	if err != nil {
		return VisionImage{}, err
	}
	if env.ArtifactID != refID {
		return VisionImage{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference artifact id does not match the envelope")
	}
	if !strings.EqualFold(env.MIME, part.FileData.MIMEType) {
		return VisionImage{}, newError(ErrorCodeProtocolInvalid, "", "", "image reference media type does not match the envelope")
	}
	return VisionImage{
		RefID:            refID,
		Digest:           env.DerivedDigest,
		MIME:             env.MIME,
		Width:            env.Width,
		Height:           env.Height,
		EncodedBytes:     env.EncodedBytes,
		TransformVersion: env.TransformVersion,
	}, nil
}

func hasVisionInput(contents []*genai.Content) bool {
	for _, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if part.FileData != nil {
				return true
			}
			if part.InlineData != nil && strings.HasPrefix(strings.ToLower(part.InlineData.MIMEType), "image/") {
				return true
			}
		}
	}
	return false
}

type VisionRequestLimits struct {
	MaxImages       int
	MaxRequestBytes int64
	MaxEncodedBytes int64
	MaxPixels       int64
	MaxDimension    int
	Detail          string
}

type VisionPolicy struct {
	global             VisionRequestLimits
	providers          map[string]VisionRequestLimits
	transformVersion   string
	maxTransformMemory int64
}

func NewVisionPolicy(cfg *config.Vision) *VisionPolicy {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	policy := &VisionPolicy{
		global: VisionRequestLimits{
			MaxImages:       cfg.MaxImages,
			MaxRequestBytes: cfg.MaxRequestBytes,
			MaxEncodedBytes: cfg.MaxEncodedBytes,
			MaxPixels:       cfg.MaxPixels,
			MaxDimension:    cfg.MaxDimension,
			Detail:          cfg.Detail,
		},
		providers:          make(map[string]VisionRequestLimits, len(cfg.Providers)),
		transformVersion:   cfg.TransformVersion,
		maxTransformMemory: int64(cfg.MaxTransformMemory),
	}
	for protocol, limits := range cfg.Providers {
		policy.providers[protocol] = VisionRequestLimits{
			MaxImages:       limits.MaxImages,
			MaxRequestBytes: limits.MaxRequestBytes,
			Detail:          limits.Detail,
		}
	}
	return policy
}

func (p *VisionPolicy) effective(protocol string) VisionRequestLimits {
	if p == nil {
		return VisionRequestLimits{}
	}
	out := p.global
	override, ok := p.providers[protocol]
	if !ok {
		return out
	}
	if override.MaxImages > 0 && override.MaxImages < out.MaxImages {
		out.MaxImages = override.MaxImages
	}
	if override.MaxRequestBytes > 0 && override.MaxRequestBytes < out.MaxRequestBytes {
		out.MaxRequestBytes = override.MaxRequestBytes
	}
	if override.Detail != "" {
		out.Detail = override.Detail
	}
	return out
}

func (p *VisionPolicy) DetailFor(protocol string) string {
	return p.effective(protocol).Detail
}

func (p *VisionPolicy) TransformVersion() string {
	if p == nil {
		return ""
	}
	return p.transformVersion
}

func (p *VisionPolicy) MaxTransformMemory() int64 {
	if p == nil {
		return 0
	}
	return p.maxTransformMemory
}

func (p *VisionPolicy) CheckGlobal(images []VisionImage) error {
	if p == nil {
		return newError(ErrorCodeProtocolInvalid, "", "", "image input is not enabled")
	}
	return checkVisionBudget(p.global, images)
}

func (p *VisionPolicy) CheckCandidate(protocol string, images []VisionImage) error {
	if p == nil {
		return newError(ErrorCodeProtocolInvalid, "", "", "image input is not enabled")
	}
	return checkVisionBudget(p.effective(protocol), images)
}

func (p *VisionPolicy) CheckGlobalPreStore(images []VisionImage) error {
	if p == nil {
		return newError(ErrorCodeProtocolInvalid, "", "", "image input is not enabled")
	}
	return checkVisionBudgetPreStore(p.global, images)
}

func (p *VisionPolicy) CheckCandidatePreStore(protocol string, images []VisionImage) error {
	if p == nil {
		return newError(ErrorCodeProtocolInvalid, "", "", "image input is not enabled")
	}
	return checkVisionBudgetPreStore(p.effective(protocol), images)
}

func checkVisionBudgetPreStore(limits VisionRequestLimits, images []VisionImage) error {
	if limits.MaxImages <= 0 || limits.MaxRequestBytes <= 0 || limits.MaxEncodedBytes <= 0 {
		return newError(ErrorCodeVisionBudgetExceeded, "", "", "image budget is not configured")
	}
	if len(images) > limits.MaxImages {
		return newError(ErrorCodeVisionBudgetExceeded, "", "", fmt.Sprintf("image count %d exceeds the maximum of %d", len(images), limits.MaxImages))
	}
	var total int64
	for i := range images {
		img := &images[i]
		if img.EncodedBytes <= 0 {
			return newError(ErrorCodeProtocolInvalid, "", "", "image reference encoded size is invalid")
		}
		if img.EncodedBytes > limits.MaxEncodedBytes {
			return newError(ErrorCodeVisionBudgetExceeded, "", "", fmt.Sprintf("image size %d exceeds the per-image maximum of %d", img.EncodedBytes, limits.MaxEncodedBytes))
		}
		if total > limits.MaxRequestBytes-img.EncodedBytes {
			return newError(ErrorCodeVisionBudgetExceeded, "", "", fmt.Sprintf("image request size exceeds the maximum of %d", limits.MaxRequestBytes))
		}
		total += img.EncodedBytes
	}
	switch limits.Detail {
	case "auto", "low", "high":
	default:
		return newError(ErrorCodeVisionBudgetExceeded, "", "", "image detail level is not supported")
	}
	return nil
}

func checkVisionBudget(limits VisionRequestLimits, images []VisionImage) error {
	if limits.MaxImages <= 0 || limits.MaxRequestBytes <= 0 || limits.MaxEncodedBytes <= 0 {
		return newError(ErrorCodeVisionBudgetExceeded, "", "", "image budget is not configured")
	}
	if len(images) > limits.MaxImages {
		return newError(ErrorCodeVisionBudgetExceeded, "", "", fmt.Sprintf("image count %d exceeds the maximum of %d", len(images), limits.MaxImages))
	}
	var total int64
	for i := range images {
		img := &images[i]
		if img.Width <= 0 || img.Height <= 0 || img.EncodedBytes <= 0 {
			return newError(ErrorCodeProtocolInvalid, "", "", "image reference dimensions are invalid")
		}
		if limits.MaxDimension > 0 && (img.Width > limits.MaxDimension || img.Height > limits.MaxDimension) {
			return newError(ErrorCodeProtocolInvalid, "", "", "image reference dimensions exceed the configured maximum")
		}
		if limits.MaxPixels > 0 && img.Width > 0 && img.Height > 0 {
			if int64(img.Width)*int64(img.Height) > limits.MaxPixels {
				return newError(ErrorCodeProtocolInvalid, "", "", "image reference pixel count exceeds the configured maximum")
			}
		}
		if img.EncodedBytes > limits.MaxEncodedBytes {
			return newError(ErrorCodeVisionBudgetExceeded, "", "", fmt.Sprintf("image size %d exceeds the per-image maximum of %d", img.EncodedBytes, limits.MaxEncodedBytes))
		}
		if total > limits.MaxRequestBytes-img.EncodedBytes {
			return newError(ErrorCodeVisionBudgetExceeded, "", "", fmt.Sprintf("image request size exceeds the maximum of %d", limits.MaxRequestBytes))
		}
		total += img.EncodedBytes
	}
	switch limits.Detail {
	case "auto", "low", "high":
	default:
		return newError(ErrorCodeVisionBudgetExceeded, "", "", "image detail level is not supported")
	}
	return nil
}

func isVisionBudgetError(err error) bool {
	code, ok := CodeOf(err)
	return ok && code == ErrorCodeVisionBudgetExceeded
}

type VisionBlobReader interface {
	ReadBlob(ctx context.Context, refID string, maxBytes int64) ([]byte, error)
}

func verifyDerivedDigest(data []byte, digest string) error {
	raw, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil || len(raw) != sha256.Size {
		return newError(ErrorCodeVisionArtifactUnavailable, "", "", "stored bytes do not match the declared reference")
	}
	sum := sha256.Sum256(data)
	if subtle.ConstantTimeCompare(sum[:], raw) != 1 {
		return newError(ErrorCodeVisionArtifactUnavailable, "", "", "stored bytes do not match the declared reference")
	}
	return nil
}

func visionWireSizeOK(total, images, maxRequestBytes int64) error {
	if total < 0 || images < 0 || maxRequestBytes <= 0 {
		return newError(ErrorCodeVisionBudgetExceeded, "", "", "image request size exceeds the maximum")
	}
	overhead := total/3 + images*4 + 1024
	if overhead < 0 || total > maxRequestBytes-overhead {
		return newError(ErrorCodeVisionBudgetExceeded, "", "", fmt.Sprintf("image request size exceeds the maximum of %d", maxRequestBytes))
	}
	return nil
}

func (c *coreClient) resolveVisionImages(ctx context.Context, req *adkmodel.LLMRequest) error {
	if ctx == nil {
		return newError(ErrorCodeProtocolInvalid, "", "", "image resolution context is missing")
	}
	images, err := collectVisionImages(req.Contents)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		return nil
	}
	if c.vision == nil || c.visionBlobs == nil {
		return newError(ErrorCodeProtocolInvalid, "", "", "image input is not enabled")
	}
	protocol := ""
	if c.codec != nil {
		protocol = c.codec.protocol()
	}
	if err := c.vision.CheckCandidatePreStore(protocol, images); err != nil {
		return err
	}
	limits := c.vision.effective(protocol)
	wantVersion := c.vision.TransformVersion()
	maxMemory := c.vision.MaxTransformMemory()
	var total int64
	for i := range images {
		if err := ctx.Err(); err != nil {
			return err
		}
		img := &images[i]
		if len(img.Data) > 0 {
			if total > limits.MaxRequestBytes-int64(len(img.Data)) {
				return newError(ErrorCodeVisionBudgetExceeded, "", "", "image request size exceeds the request maximum")
			}
			total += int64(len(img.Data))
			continue
		}
		data, err := c.visionBlobs.ReadBlob(ctx, img.RefID, limits.MaxEncodedBytes)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return codedError(ErrorCodeVisionArtifactUnavailable, err, "stored image is not available")
		}
		if len(data) == 0 || int64(len(data)) != img.EncodedBytes {
			return newError(ErrorCodeVisionArtifactUnavailable, "", "", "stored bytes do not match the declared reference")
		}
		if err := verifyDerivedDigest(data, img.Digest); err != nil {
			return err
		}
		if wantVersion != "" && vision.TransformSatisfies(img.Width, img.Height, limits.MaxPixels, limits.MaxDimension, img.TransformVersion, wantVersion) {
			if total > limits.MaxRequestBytes-int64(len(data)) {
				return newError(ErrorCodeVisionBudgetExceeded, "", "", "image request size exceeds the request maximum")
			}
			total += int64(len(data))
			img.Data = data
			continue
		}
		rederived, newW, newH, newDigest, err := rederiveVisionBytes(ctx, data, img.MIME, limits, maxMemory)
		if err != nil {
			return err
		}
		if total > limits.MaxRequestBytes-int64(len(rederived)) {
			return newError(ErrorCodeVisionBudgetExceeded, "", "", "image request size exceeds the request maximum")
		}
		total += int64(len(rederived))
		img.Data = rederived
		img.Width = newW
		img.Height = newH
		img.EncodedBytes = int64(len(rederived))
		img.Digest = newDigest
		img.TransformVersion = wantVersion
	}
	if err := checkVisionBudget(limits, images); err != nil {
		return err
	}
	if err := visionWireSizeOK(total, int64(len(images)), limits.MaxRequestBytes); err != nil {
		return err
	}
	applyResolvedImages(req, images)
	return nil
}

func decodeVisionBytes(data []byte, mime string) (image.Image, error) {
	switch strings.ToLower(mime) {
	case "image/png":
		return png.Decode(bytes.NewReader(data))
	case "image/jpeg":
		return jpeg.Decode(bytes.NewReader(data))
	case "image/gif":
		all, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if len(all.Image) == 0 {
			return nil, errNoGifFrames
		}
		return all.Image[0], nil
	case "image/webp":
		return webp.Decode(bytes.NewReader(data))
	default:
		return nil, errUnsupportedVisionMedia
	}
}

func encodeVisionBytes(img image.Image, mime string) ([]byte, error) {
	var buf bytes.Buffer
	switch strings.ToLower(mime) {
	case "image/jpeg":
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

func mapVisionTransformError(err error) error {
	if err == nil {
		return nil
	}
	if code, ok := vision.CodeOf(err); ok {
		switch code {
		case vision.ErrorCodeVisionLimitExceeded, vision.ErrorCodeVisionBudgetExceeded:
			return codedError(ErrorCodeVisionBudgetExceeded, err, "stored image exceeds the configured maximum")
		case vision.ErrorCodeVisionDecodeFailed, vision.ErrorCodeVisionArtifactUnavailable, vision.ErrorCodeVisionInvalid:
			return codedError(ErrorCodeVisionArtifactUnavailable, err, "stored image is not available")
		case vision.ErrorCodeInvalidArgument, vision.ErrorCodeVisionFormatUnsupported:
			return codedError(ErrorCodeProtocolInvalid, err, "stored image transform did not complete")
		}
	}
	return codedError(ErrorCodeProtocolInvalid, err, "stored image transform did not complete")
}

func rederiveVisionBytes(ctx context.Context, data []byte, mime string, limits VisionRequestLimits, maxMemory int64) (redrived []byte, newW, newH int, newDigest string, err error) {
	decoded, err := decodeVisionBytes(data, mime)
	if err != nil {
		return nil, 0, 0, "", codedError(ErrorCodeVisionArtifactUnavailable, err, "stored image is not available")
	}
	bounds := decoded.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, 0, 0, "", newError(ErrorCodeVisionArtifactUnavailable, "", "", "stored bytes do not match the declared reference")
	}
	dstW, dstH, resized, err := vision.PlanGeometry(srcW, srcH, limits.MaxPixels, limits.MaxDimension)
	if err != nil {
		return nil, 0, 0, "", mapVisionTransformError(err)
	}
	out := decoded
	if resized {
		if maxMemory <= 0 {
			return nil, 0, 0, "", newError(ErrorCodeVisionBudgetExceeded, "", "", "image transform budget is not configured")
		}
		resampled, err := vision.ResizeLanczos3(ctx, decoded, dstW, dstH, maxMemory)
		if err != nil {
			return nil, 0, 0, "", mapVisionTransformError(err)
		}
		out = resampled
	}
	encoded, err := encodeVisionBytes(out, mime)
	if err != nil {
		return nil, 0, 0, "", codedError(ErrorCodeVisionArtifactUnavailable, err, "stored image is not available")
	}
	sum := sha256.Sum256(encoded)
	return encoded, dstW, dstH, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func applyResolvedImages(req *adkmodel.LLMRequest, images []VisionImage) {
	next := 0
	for _, content := range req.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			switch {
			case part.FileData != nil && strings.HasPrefix(part.FileData.FileURI, visionArtifactScheme):
				if next < len(images) && len(images[next].Data) > 0 {
					part.InlineData = &genai.Blob{MIMEType: images[next].MIME, Data: images[next].Data}
					part.FileData = nil
				}
				next++
			case part.InlineData != nil && strings.HasPrefix(strings.ToLower(part.InlineData.MIMEType), "image/"):
				next++
			}
		}
	}
}

func visionDataURI(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func normalizeVisionDetail(detail string) string {
	switch detail {
	case "low", "high", "auto":
		return detail
	default:
		return "auto"
	}
}

func PredicateVisionImages(req *adkmodel.LLMRequest) bool {
	if req == nil {
		return false
	}
	return hasVisionInput(req.Contents)
}

func mapVisionExhaustion(route string, errs []candidateAttemptError) error {
	var unsupported, budget bool
	var firstBudget error
	for _, ce := range errs {
		switch {
		case ce.Class == ErrorClassUnsupported:
			unsupported = true
		case isVisionBudgetError(ce.Err):
			budget = true
			if firstBudget == nil {
				firstBudget = ce.Err
			}
		case ce.Class.FallbackEligible():
			return nil
		default:
			return nil
		}
	}
	switch {
	case unsupported:
		return newError(ErrorCodeCapabilityUnsupported, route, "vision", "route has no vision-capable candidate")
	case budget:
		return firstBudget
	default:
		return nil
	}
}
