package proxy

import (
	"errors"
	"io"
	"sync"
	"time"
)

var errUpstreamIdleTimeout = errors.New("upstream media read idle timeout")

type idleTimeoutReadCloser struct {
	body     io.ReadCloser
	timeout  time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	closed   bool
	timedOut bool
}

func newIdleTimeoutReadCloser(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return body
	}
	reader := &idleTimeoutReadCloser{body: body, timeout: timeout}
	reader.timer = time.AfterFunc(timeout, reader.expire)
	return reader
}

func (r *idleTimeoutReadCloser) Read(buffer []byte) (int, error) {
	count, err := r.body.Read(buffer)
	r.mu.Lock()
	timedOut := r.timedOut
	if count > 0 && !r.closed && !timedOut {
		r.timer.Reset(r.timeout)
	}
	r.mu.Unlock()
	if timedOut {
		return 0, errUpstreamIdleTimeout
	}
	return count, err
}

func (r *idleTimeoutReadCloser) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.body.Close()
}

func (r *idleTimeoutReadCloser) expire() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.timedOut = true
	r.closed = true
	r.mu.Unlock()
	_ = r.body.Close()
}
