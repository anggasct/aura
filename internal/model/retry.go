package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type RetryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	Jitter     float64
	Sleep      func(context.Context, time.Duration) error
	RetryCount *int
	Budget     *InvocationBudget
}

func defaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		BaseDelay:  time.Second,
		MaxDelay:   60 * time.Second,
		Jitter:     0.25,
		Sleep:      sleepWithContext,
	}
}

func (c RetryConfig) normalized() RetryConfig {
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = time.Second
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = 60 * time.Second
	}
	if c.Jitter < 0 {
		c.Jitter = 0
	}
	if c.Sleep == nil {
		c.Sleep = sleepWithContext
	}
	return c
}

func retryHTTP(ctx context.Context, cfg RetryConfig, do func() (*http.Response, error)) (*http.Response, error) {
	cfg = cfg.normalized()
	for retry := 0; ; retry++ {
		if cfg.Budget != nil {
			if err := cfg.Budget.RecordAttempt(ctx); err != nil {
				return nil, err
			}
		}
		resp, err := do()
		if err != nil {
			// Transient transport failures are retried like retryable
			// statuses; everything else (auth, protocol, context) is not.
			if !isTransientError(err) || ctx.Err() != nil || retry >= cfg.MaxRetries {
				return nil, err
			}
			delay := exponentialDelay(cfg, retry)
			if cfg.Budget != nil {
				var bErr error
				delay, bErr = cfg.Budget.ReserveDelay(ctx, delay)
				if bErr != nil {
					return nil, bErr
				}
			}
			if err := cfg.Sleep(ctx, delay); err != nil {
				return nil, err
			}
			if cfg.RetryCount != nil {
				*cfg.RetryCount = retry + 1
			}
			continue
		}
		if !isRetryableStatus(resp.StatusCode) || retry >= cfg.MaxRetries {
			return resp, nil
		}

		delay, hasRetryAfter := retryAfter(resp.Header.Get("Retry-After"))
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		if !hasRetryAfter {
			delay = exponentialDelay(cfg, retry)
		} else if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
		if cfg.Budget != nil {
			var bErr error
			delay, bErr = cfg.Budget.ReserveDelay(ctx, delay)
			if bErr != nil {
				return nil, bErr
			}
		}
		if err := cfg.Sleep(ctx, delay); err != nil {
			return nil, err
		}
		if cfg.RetryCount != nil {
			*cfg.RetryCount = retry + 1
		}
	}
}

func isRetryableStatus(status int) bool {
	switch {
	case status == http.StatusRequestTimeout, status == 425, status == http.StatusTooManyRequests:
		return true
	case status >= 500 && status <= 599:
		return true
	}
	return false
}

// providerErrorBody captures the error object shape shared by the providers:
// OpenAI/Responses (code/type/message), Anthropic (type/message), Gemini
// (status/message).
type providerErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"error"`
}

func isTransientError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func classifyHTTPResponse(resp *http.Response, provider string) error {
	if resp.StatusCode != http.StatusBadRequest {
		return classifyHTTPStatus(resp.StatusCode, provider)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return fmt.Errorf("%s: http 400", provider)
	}
	var payload providerErrorBody
	_ = json.Unmarshal(body, &payload)
	code := strings.ToLower(payload.Error.Code)
	typ := strings.ToLower(payload.Error.Type)
	status := strings.ToLower(payload.Error.Status)
	message := strings.ToLower(payload.Error.Message)
	switch {
	case strings.Contains(code, "content_filter"), strings.Contains(typ, "content_filter"), strings.Contains(status, "blocked"), strings.Contains(message, "content filter"), strings.Contains(message, "safety settings"):
		return codedError(ErrorCodeContentFiltered, ErrContentFiltered, provider+": http 400")
	case strings.Contains(code, "context_length"), strings.Contains(typ, "context_length"), strings.Contains(message, "context_length"), strings.Contains(message, "too long"):
		return codedError(ErrorCodeContextTooLong, ErrContextTooLong, provider+": http 400")
	}
	return fmt.Errorf("%s: http 400", provider)
}

func exponentialDelay(cfg RetryConfig, retry int) time.Duration {
	delay := cfg.BaseDelay
	for i := 0; i < retry && delay < cfg.MaxDelay; i++ {
		if delay > cfg.MaxDelay/2 {
			delay = cfg.MaxDelay
			break
		}
		delay *= 2
	}
	if delay > cfg.MaxDelay {
		delay = cfg.MaxDelay
	}
	if cfg.Jitter > 0 {
		factor := 1 + (rand.Float64()*2-1)*cfg.Jitter
		delay = time.Duration(float64(delay) * factor)
		if delay < 0 {
			delay = 0
		}
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
	}
	return delay
}

func retryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := time.Until(when)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
