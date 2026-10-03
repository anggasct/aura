package vision

import (
	stdcontext "context"
	"time"
)

type Observation struct {
	Operation    string
	MIME         string
	Images       int
	EncodedBytes int64
	Pixels       int64
	Width        int
	Height       int
	Version      string
	Protocol     string
	Result       string
	Duration     time.Duration
	Err          error
}

type Observer func(ctx stdcontext.Context, observation *Observation)

func (s *Service) observe(ctx stdcontext.Context, observation *Observation) {
	if s == nil || s.observer == nil || observation == nil {
		return
	}
	if ctx == nil {
		return
	}
	if observation.Err != nil {
		if code, ok := CodeOf(observation.Err); ok {
			observation.Result = string(code)
		} else {
			observation.Result = "error"
		}
	} else {
		observation.Result = "ok"
	}
	observation.Operation = sanitizeOperation(observation.Operation)
	observation.MIME = sanitizeMIME(observation.MIME)
	observation.Protocol = sanitizeProtocol(observation.Protocol)
	observation.EncodedBytes = bucketBytes(observation.EncodedBytes)
	observation.Pixels = bucketPixels(observation.Pixels)
	observation.Width = 0
	observation.Height = 0
	s.observer(ctx, observation)
}

func sanitizeOperation(op string) string {
	switch op {
	case "ingest", "transform", "route", "deliver":
		return op
	default:
		return "ingest"
	}
}

func sanitizeMIME(mime string) string {
	switch mime {
	case MIMEPNG, MIMEJPEG, MIMEWebP, MIMEgif:
		return mime
	default:
		return ""
	}
}

func sanitizeProtocol(protocol string) string {
	switch protocol {
	case "openai_responses", "openai_chat_compat", "anthropic_messages", "gemini_native", "":
		return protocol
	default:
		return ""
	}
}

func bucketBytes(n int64) int64 {
	switch {
	case n <= 1<<16:
		return 1 << 16
	case n <= 1<<18:
		return 1 << 18
	case n <= 1<<20:
		return 1 << 20
	case n <= 1<<22:
		return 1 << 22
	default:
		return 1 << 24
	}
}

func bucketPixels(n int64) int64 {
	switch {
	case n <= 1<<18:
		return 1 << 18
	case n <= 1<<20:
		return 1 << 20
	case n <= 1<<22:
		return 1 << 22
	default:
		return 1 << 24
	}
}
