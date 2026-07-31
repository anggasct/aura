package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (valid: debug, info, warn, error)", s)
	}
}

func ParseFormat(s string) (string, error) {
	switch strings.ToLower(s) {
	case "text", "json":
		return strings.ToLower(s), nil
	default:
		return "", fmt.Errorf("invalid log format %q (valid: text, json)", s)
	}
}

func New(level, format string, w io.Writer) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	name, err := ParseFormat(format)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if name == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h), nil
}

func Setup(level, format string, w io.Writer) error {
	l, err := New(level, format, w)
	if err != nil {
		return err
	}
	slog.SetDefault(l)
	return nil
}
