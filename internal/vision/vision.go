package vision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/config"
)

const (
	PartKindImageRef = "image_ref.v1"

	TrustUntrustedExternal = "untrusted_external"

	maxSniffBytes   = 64 << 10
	maxAltTextChars = 512
	maxHeaderBytes  = 1 << 20
)

type Provenance struct {
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
	SessionID  string `json:"session_id"`
	TurnID     string `json:"turn_id"`
	IngestedAt string `json:"ingested_at"`
}

type TransformGeometry struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Upscaled bool   `json:"upscaled"`
	Kernel   string `json:"kernel"`
}

type ImagePart struct {
	Kind             string            `json:"kind"`
	ArtifactID       string            `json:"artifact_id"`
	SourceDigest     string            `json:"source_digest"`
	DerivedDigest    string            `json:"derived_digest"`
	MIME             string            `json:"mime"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	EncodedBytes     int64             `json:"encoded_bytes"`
	TransformVersion string            `json:"transform_version"`
	Transform        TransformGeometry `json:"transform"`
	Provenance       Provenance        `json:"provenance"`
	AltText          string            `json:"alt_text,omitempty"`
	Trust            string            `json:"trust"`
}

type IngestRequest struct {
	Content      io.Reader
	DeclaredMIME string
	Filename     string
	AltText      string
	SessionID    string
	Provenance   Provenance
}

type ArtifactWriter interface {
	Put(ctx context.Context, r io.Reader, meta *ArtifactMetadata) (ArtifactRef, error)
}

type ArtifactMetadata struct {
	ID        string
	SessionID string
	EventID   string
	Filename  string
	MediaType string
	Metadata  json.RawMessage
}

type ArtifactRef struct {
	ID         string
	BlobDigest string
	SizeBytes  int64
}

type Limits struct {
	MaxEncodedBytes      int64
	MaxPixels            int64
	MaxDimension         int
	DecodeTimeout        time.Duration
	MaxDecodeConcurrency int
	MaxTransformMemory   int64
	StripMetadata        bool
	OrientNormalize      bool
	TransformVersion     string
	Now                  func() time.Time
}

type decodedImage struct {
	mime   string
	width  int
	height int
	pixels image.Image
}

type Service struct {
	limits   Limits
	store    ArtifactWriter
	sem      chan struct{}
	observer Observer
}

type Option func(*Service)

func WithStore(store ArtifactWriter) Option {
	return func(s *Service) { s.store = store }
}

func WithObserver(observer Observer) Option {
	return func(s *Service) { s.observer = observer }
}

func NewService(limits *Limits, opts ...Option) (*Service, error) {
	if limits == nil {
		return nil, errNilArgument("limits")
	}
	if limits.MaxEncodedBytes <= 0 || limits.MaxPixels <= 0 || limits.MaxDimension <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "vision limits must be positive")
	}
	if limits.DecodeTimeout <= 0 || limits.DecodeTimeout > 30*time.Second {
		return nil, Errorf(ErrorCodeInvalidArgument, "vision decode timeout is out of range")
	}
	if limits.MaxDecodeConcurrency <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "vision decode concurrency must be positive")
	}
	if limits.MaxTransformMemory <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "vision transform memory must be positive")
	}
	if strings.TrimSpace(limits.TransformVersion) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "vision transform version must not be empty")
	}
	s := &Service{limits: *limits, sem: make(chan struct{}, limits.MaxDecodeConcurrency)}
	for _, opt := range opts {
		if opt == nil {
			return nil, errNilArgument("option")
		}
		opt(s)
	}
	return s, nil
}

func MaxFrames() int { return 1 }

func LimitsFromConfig(cfg *config.Vision) Limits {
	if cfg == nil {
		return Limits{}
	}
	return Limits{
		MaxEncodedBytes:      cfg.MaxEncodedBytes,
		MaxPixels:            cfg.MaxPixels,
		MaxDimension:         cfg.MaxDimension,
		DecodeTimeout:        time.Duration(cfg.DecodeTimeout),
		MaxDecodeConcurrency: cfg.MaxDecodeConcurrency,
		MaxTransformMemory:   int64(cfg.MaxTransformMemory),
		StripMetadata:        cfg.StripMetadata,
		OrientNormalize:      cfg.OrientNormalize,
		TransformVersion:     cfg.TransformVersion,
		Now:                  time.Now,
	}
}

func (s *Service) Ingest(ctx context.Context, req *IngestRequest) (ImagePart, error) {
	started := time.Now()
	part, err := s.ingest(ctx, req)
	s.observe(ctx, &Observation{
		Operation:    "ingest",
		MIME:         part.MIME,
		Images:       boolToImages(err == nil),
		EncodedBytes: part.EncodedBytes,
		Pixels:       int64(part.Width) * int64(part.Height),
		Width:        part.Width,
		Height:       part.Height,
		Version:      s.limits.TransformVersion,
		Duration:     time.Since(started),
		Err:          err,
	})
	return part, err
}

func boolToImages(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

func (s *Service) ingest(ctx context.Context, req *IngestRequest) (ImagePart, error) {
	if ctx == nil {
		return ImagePart{}, errNilArgument("context")
	}
	if req == nil {
		return ImagePart{}, errNilArgument("ingest request")
	}
	if req.Content == nil {
		return ImagePart{}, Errorf(ErrorCodeInvalidArgument, "image content must not be nil")
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return ImagePart{}, Errorf(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	if s.store == nil {
		return ImagePart{}, Errorf(ErrorCodeInvalidArgument, "artifact store must not be nil")
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return ImagePart{}, Errorf(ErrorCodeVisionLimitExceeded, "decode concurrency is exhausted")
	}
	transferred := false
	defer func() {
		if !transferred {
			<-s.sem
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(req.Content, s.limits.MaxEncodedBytes+1))
	if err != nil {
		return ImagePart{}, Errorf(ErrorCodeVisionDecodeFailed, "image read did not complete")
	}
	if int64(len(raw)) > s.limits.MaxEncodedBytes {
		return ImagePart{}, Errorf(ErrorCodeVisionLimitExceeded, "image exceeds the encoded byte limit")
	}
	if len(raw) == 0 {
		return ImagePart{}, Errorf(ErrorCodeVisionInvalid, "image is empty")
	}
	prefixLen := clampPrefix(len(raw), sniffPrefixLen)
	mime, ok := sniffFormat(raw[:prefixLen])
	if !ok {
		return ImagePart{}, Errorf(ErrorCodeVisionFormatUnsupported, "image format is not supported")
	}
	if declared := normalizeDeclaredMIME(req.DeclaredMIME); declared != "" && declared != mime {
		return ImagePart{}, Errorf(ErrorCodeVisionInvalid, "declared media type does not match sniffed bytes")
	}
	geometry, err := sniffDimensions(mime, raw)
	if err != nil {
		return ImagePart{}, err
	}
	if geometry.animated || geometry.frames > 1 {
		return ImagePart{}, Errorf(ErrorCodeVisionLimitExceeded, "animated images beyond the first frame are not supported")
	}
	if err := checkGeometry(geometry.width, geometry.height, &s.limits); err != nil {
		return ImagePart{}, err
	}

	decoded, trans, err := s.decodeBounded(ctx, mime, raw)
	transferred = trans
	if err != nil {
		return ImagePart{}, err
	}
	oriented := applyOrientation(decoded.pixels, orientationOf(mime, raw), s.limits.OrientNormalize)
	bounds := oriented.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if err := checkGeometry(width, height, &s.limits); err != nil {
		return ImagePart{}, err
	}
	encoded, err := encodeNormalized(mime, oriented)
	if err != nil {
		return ImagePart{}, err
	}
	if int64(len(encoded)) > s.limits.MaxEncodedBytes {
		return ImagePart{}, Errorf(ErrorCodeVisionLimitExceeded, "normalized image exceeds the encoded byte limit")
	}

	sourceDigest := "sha256:" + hex.EncodeToString(sha256sum(raw))
	derivedDigest := "sha256:" + hex.EncodeToString(sha256sum(encoded))
	now := time.Now().UTC()
	if s.limits.Now != nil {
		now = s.limits.Now().UTC()
	}
	provenance := req.Provenance
	provenance.SessionID = req.SessionID
	provenance.IngestedAt = now.Format(time.RFC3339Nano)
	altText, err := normalizeAltText(req.AltText)
	if err != nil {
		return ImagePart{}, err
	}
	part := ImagePart{
		Kind:             PartKindImageRef,
		SourceDigest:     sourceDigest,
		DerivedDigest:    derivedDigest,
		MIME:             mime,
		Width:            width,
		Height:           height,
		EncodedBytes:     int64(len(encoded)),
		TransformVersion: s.limits.TransformVersion,
		Transform:        TransformGeometry{Width: width, Height: height, Upscaled: false, Kernel: transformKernel()},
		Provenance:       provenance,
		AltText:          altText,
		Trust:            TrustUntrustedExternal,
	}

	sourceMeta, err := partMetadata(&part, "source")
	if err != nil {
		return ImagePart{}, err
	}
	derivedMeta, err := partMetadata(&part, "derived")
	if err != nil {
		return ImagePart{}, err
	}
	sourceRef, err := s.store.Put(ctx, bytes.NewReader(raw), &ArtifactMetadata{
		SessionID: req.SessionID,
		Filename:  req.Filename,
		MediaType: mime,
		Metadata:  sourceMeta,
	})
	if err != nil {
		return ImagePart{}, Errorf(ErrorCodeVisionArtifactUnavailable, "source artifact write did not complete")
	}
	_ = sourceRef
	derivedRef, err := s.store.Put(ctx, bytes.NewReader(encoded), &ArtifactMetadata{
		SessionID: req.SessionID,
		Filename:  req.Filename,
		MediaType: derivedMediaType(mime),
		Metadata:  derivedMeta,
	})
	if err != nil {
		return ImagePart{}, Errorf(ErrorCodeVisionArtifactUnavailable, "derived artifact write did not complete")
	}
	if derivedRef.ID == "" {
		return ImagePart{}, Errorf(ErrorCodeVisionArtifactUnavailable, "derived artifact write did not complete")
	}
	part.ArtifactID = derivedRef.ID
	if err := ValidatePart(&part); err != nil {
		return ImagePart{}, err
	}
	return part, nil
}

func checkGeometry(width, height int, limits *Limits) error {
	if width <= 0 || height <= 0 {
		return Errorf(ErrorCodeVisionDecodeFailed, "image dimensions are invalid")
	}
	if width > limits.MaxDimension || height > limits.MaxDimension {
		return Errorf(ErrorCodeVisionLimitExceeded, "image dimension exceeds the configured maximum")
	}
	pixels := int64(width) * int64(height)
	if pixels > limits.MaxPixels {
		return Errorf(ErrorCodeVisionLimitExceeded, "image pixel count exceeds the configured maximum")
	}
	if pixels*4 > limits.MaxTransformMemory {
		return Errorf(ErrorCodeVisionLimitExceeded, "decoded image exceeds the transform memory bound")
	}
	return nil
}

func (s *Service) decodeBounded(ctx context.Context, mime string, raw []byte) (decodedImage, bool, error) {
	type result struct {
		img image.Image
		err error
	}
	done := make(chan result, 1)
	go func() {
		var img image.Image
		var err error
		switch mime {
		case MIMEPNG:
			img, err = png.Decode(bytes.NewReader(raw))
		case MIMEJPEG:
			img, err = jpeg.Decode(bytes.NewReader(raw))
		case MIMEgif:
			var gifImg *gif.GIF
			gifImg, err = gif.DecodeAll(bytes.NewReader(raw))
			if err == nil {
				if len(gifImg.Image) == 0 {
					err = Errorf(ErrorCodeVisionDecodeFailed, "gif has no frames")
				} else {
					img = gifImg.Image[0]
				}
			}
		case MIMEWebP:
			img, err = decodeWebP(raw)
		default:
			err = Errorf(ErrorCodeVisionFormatUnsupported, "image format is not supported")
		}
		done <- result{img: img, err: err}
	}()
	timeout := s.limits.DecodeTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		go func() {
			<-done
			<-s.sem
		}()
		return decodedImage{}, true, Errorf(ErrorCodeVisionDecodeFailed, "image decode was cancelled")
	case <-timer.C:
		go func() {
			<-done
			<-s.sem
		}()
		return decodedImage{}, true, Errorf(ErrorCodeVisionDecodeFailed, "image decode exceeded the deadline")
	case res := <-done:
		if res.err != nil {
			return decodedImage{}, false, Errorf(ErrorCodeVisionDecodeFailed, "image decode did not complete")
		}
		if res.img == nil {
			return decodedImage{}, false, Errorf(ErrorCodeVisionDecodeFailed, "image decode produced no pixels")
		}
		bounds := res.img.Bounds()
		return decodedImage{mime: mime, width: bounds.Dx(), height: bounds.Dy(), pixels: res.img}, false, nil
	}
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func normalizeAltText(alt string) (string, error) {
	if alt == "" {
		return "", nil
	}
	if !utf8.ValidString(alt) {
		return "", Errorf(ErrorCodeInvalidArgument, "alt text is not valid utf-8")
	}
	runes := []rune(alt)
	if len(runes) > maxAltTextChars {
		return "", Errorf(ErrorCodeInvalidArgument, "alt text exceeds the character limit")
	}
	return strings.TrimSpace(alt), nil
}

func partMetadata(part *ImagePart, role string) (json.RawMessage, error) {
	raw, err := json.Marshal(map[string]any{
		"role":              role,
		"kind":              part.Kind,
		"source_digest":     part.SourceDigest,
		"derived_digest":    part.DerivedDigest,
		"mime":              part.MIME,
		"width":             part.Width,
		"height":            part.Height,
		"transform_version": part.TransformVersion,
	})
	if err != nil {
		return nil, Errorf(ErrorCodeVisionArtifactUnavailable, "artifact metadata did not encode")
	}
	return raw, nil
}

func transformKernel() string {
	return "lanczos3"
}

func derivedMediaType(mime string) string {
	if mime == MIMEJPEG {
		return MIMEJPEG
	}
	return MIMEPNG
}

func clampPrefix(length, bound int) int {
	if length < bound {
		return length
	}
	return bound
}

func ValidatePart(part *ImagePart) error {
	if part == nil {
		return errNilArgument("image part")
	}
	if part.Kind != PartKindImageRef {
		return Errorf(ErrorCodeInvalidArgument, "image part kind is not supported")
	}
	if strings.TrimSpace(part.ArtifactID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "image part artifact id must not be empty")
	}
	if !strings.HasPrefix(part.SourceDigest, "sha256:") || !strings.HasPrefix(part.DerivedDigest, "sha256:") {
		return Errorf(ErrorCodeInvalidArgument, "image part digests must use sha256")
	}
	switch part.MIME {
	case MIMEPNG, MIMEJPEG, MIMEWebP, MIMEgif:
	default:
		return Errorf(ErrorCodeInvalidArgument, "image part mime is not supported")
	}
	if part.Width <= 0 || part.Height <= 0 {
		return Errorf(ErrorCodeInvalidArgument, "image part dimensions are invalid")
	}
	if part.EncodedBytes <= 0 {
		return Errorf(ErrorCodeInvalidArgument, "image part byte count is invalid")
	}
	if strings.TrimSpace(part.TransformVersion) == "" {
		return Errorf(ErrorCodeInvalidArgument, "image part transform version must not be empty")
	}
	if part.Transform.Width != part.Width || part.Transform.Height != part.Height {
		return Errorf(ErrorCodeInvalidArgument, "image part transform geometry does not match dimensions")
	}
	if part.Transform.Upscaled {
		return Errorf(ErrorCodeInvalidArgument, "image part must not upscale")
	}
	if part.TransformVersion == "v1" {
		if part.Transform.Kernel != "lanczos3" {
			return Errorf(ErrorCodeInvalidArgument, "image part transform kernel is not supported")
		}
	} else if strings.TrimSpace(part.Transform.Kernel) == "" {
		return Errorf(ErrorCodeInvalidArgument, "image part transform kernel must not be empty")
	}
	if strings.TrimSpace(part.Provenance.Source) == "" {
		return Errorf(ErrorCodeInvalidArgument, "image part provenance source must not be empty")
	}
	if strings.TrimSpace(part.Provenance.SessionID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "image part provenance session must not be empty")
	}
	if strings.TrimSpace(part.Provenance.TurnID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "image part provenance turn must not be empty")
	}
	if strings.TrimSpace(part.Provenance.IngestedAt) == "" {
		return Errorf(ErrorCodeInvalidArgument, "image part provenance timestamp must not be empty")
	}
	if part.Trust != TrustUntrustedExternal {
		return Errorf(ErrorCodeInvalidArgument, "image part trust must be untrusted_external")
	}
	if len([]rune(part.AltText)) > maxAltTextChars {
		return Errorf(ErrorCodeInvalidArgument, "alt text exceeds the character limit")
	}
	return nil
}

func MarshalPart(part *ImagePart) ([]byte, error) {
	if err := ValidatePart(part); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(part)
	if err != nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "image part is not serializable")
	}
	return raw, nil
}

func UnmarshalPart(raw []byte) (ImagePart, error) {
	if len(raw) == 0 {
		return ImagePart{}, Errorf(ErrorCodeInvalidArgument, "image part is empty")
	}
	if len(raw) > maxHeaderBytes {
		return ImagePart{}, Errorf(ErrorCodeInvalidArgument, "image part exceeds the size bound")
	}
	var part ImagePart
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&part); err != nil {
		return ImagePart{}, Errorf(ErrorCodeInvalidArgument, "image part is not decodable")
	}
	if err := ValidatePart(&part); err != nil {
		return ImagePart{}, err
	}
	return part, nil
}
