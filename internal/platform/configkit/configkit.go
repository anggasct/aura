package configkit

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type ByteSize int64

func (b ByteSize) MarshalText() ([]byte, error) {
	return []byte(b.String()), nil
}

func (b *ByteSize) UnmarshalText(text []byte) error {
	value, err := ParseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = value
	return nil
}

func (b ByteSize) String() string {
	switch {
	case b >= 1<<40 && b%(1<<40) == 0:
		return fmt.Sprintf("%dTiB", b/(1<<40))
	case b >= 1<<30 && b%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", b/(1<<30))
	case b >= 1<<20 && b%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", b/(1<<20))
	case b >= 1<<10 && b%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", b/(1<<10))
	default:
		return strconv.FormatInt(int64(b), 10)
	}
}

func ParseByteSize(raw string) (ByteSize, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("empty size")
	}
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	}
	for _, s := range suffixes {
		if !strings.HasSuffix(raw, s.suffix) {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(raw, s.suffix)), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid size %q", raw)
		}
		if n > math.MaxInt64/s.mult {
			return 0, fmt.Errorf("size %q overflows", raw)
		}
		return ByteSize(n * s.mult), nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", raw)
	}
	return ByteSize(n), nil
}

type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(text []byte) error {
	duration, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(duration)
	return nil
}
