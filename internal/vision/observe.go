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
	observation.EncodedBytes = bucketBytes(observation.EncodedBytes)
	observation.Pixels = bucketPixels(observation.Pixels)
	observation.Width = 0
	observation.Height = 0
	s.observer(ctx, observation)
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
