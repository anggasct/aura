package model

import (
	"bufio"
	"context"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

var errStopYield = io.EOF

type idleReader struct {
	io.ReadCloser
	activity chan struct{}
	timedOut atomic.Bool
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		select {
		case r.activity <- struct{}{}:
		default:
		}
	}
	return n, err
}

func scanSSE(ctx context.Context, body io.ReadCloser, idleTimeout time.Duration, onData func(data []byte) error) error {
	reader := &idleReader{ReadCloser: body, activity: make(chan struct{}, 1)}
	stop := make(chan struct{})
	defer close(stop)
	go monitorIdleReader(ctx, reader, idleTimeout, stop)

	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		joined := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		return onData([]byte(joined))
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			dataLines = append(dataLines, payload)
		}
	}
	if err := sc.Err(); err != nil {
		if reader.timedOut.Load() {
			return ErrStreamIdle
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return flush()
}

func monitorIdleReader(ctx context.Context, reader *idleReader, timeout time.Duration, stop <-chan struct{}) {
	if timeout <= 0 {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(timeout)
	}
	for {
		select {
		case <-ctx.Done():
			_ = reader.Close()
			return
		case <-reader.activity:
			reset()
		case <-timer.C:
			reader.timedOut.Store(true)
			_ = reader.Close()
			return
		case <-stop:
			return
		}
	}
}
