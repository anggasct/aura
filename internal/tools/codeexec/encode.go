package codeexec

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

type encodedResult struct {
	ResultClass     ResultClass `json:"result_class"`
	ExitCode        int         `json:"exit_code"`
	Signal          int         `json:"signal"`
	DurationMS      int64       `json:"duration_ms"`
	Artifacts       []string    `json:"artifacts"`
	StdoutTruncated bool        `json:"stdout_truncated"`
	StderrTruncated bool        `json:"stderr_truncated"`
	Stdout          string      `json:"stdout"`
	Stderr          string      `json:"stderr"`
}

func encodeBounded(outcome *Outcome, limits *Limits, maxBytes int64) (json.RawMessage, error) {
	if maxBytes <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "the result size limit must be positive")
	}
	result := encodedResult{
		ResultClass: outcome.Class, ExitCode: outcome.ExitCode, Signal: outcome.Signal,
		DurationMS:      outcome.Duration.Milliseconds(),
		Artifacts:       []string{},
		StdoutTruncated: outcome.StdoutTruncated, StderrTruncated: outcome.StderrTruncated,
	}
	var cut bool
	result.Stdout, cut = capText(outcome.Stdout, limits.MaxStdoutBytes)
	result.StdoutTruncated = result.StdoutTruncated || cut
	result.Stderr, cut = capText(outcome.Stderr, limits.MaxStderrBytes)
	result.StderrTruncated = result.StderrTruncated || cut
	for {
		encoded, err := marshal(&result)
		if err != nil {
			return nil, err
		}
		overflow := int64(len(encoded)) - maxBytes
		if overflow <= 0 {
			return encoded, nil
		}
		if result.Stdout == "" && result.Stderr == "" {
			return nil, Errorf(ErrorCodeInvalidArgument, "the result envelope exceeds the result size limit")
		}
		if len(result.Stdout) >= len(result.Stderr) {
			result.Stdout, _ = capText(result.Stdout, int64(len(result.Stdout))-overflow)
			result.StdoutTruncated = true
		} else {
			result.Stderr, _ = capText(result.Stderr, int64(len(result.Stderr))-overflow)
			result.StderrTruncated = true
		}
	}
}

func capText(text string, limit int64) (string, bool) {
	text = strings.ToValidUTF8(text, "�")
	if int64(len(text)) <= limit {
		return text, false
	}
	cut := max(int(limit), 0)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

func marshal(result *encodedResult) (json.RawMessage, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(result); err != nil {
		return nil, wrapf(ErrorCodeRunFailed, err, "encode the result")
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}
