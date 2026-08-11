package proxy

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	maxPlaylistSize = 4 << 20
	copyBufferSize  = 32 << 10
)

var copyBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, copyBufferSize)
		return &buffer
	},
}

type server struct {
	token           string
	publicURL       string
	mediaHosts      []string
	client          *http.Client
	logger          *log.Logger
	now             func() time.Time
	wbiMu           sync.Mutex
	wbiMixinKey     string
	wbiKeyExpires   time.Time
	wbiRefresh      chan struct{}
	wbiFetchTimeout time.Duration
}

type requestLog struct {
	route          string
	targetHost     string
	upstreamStatus int
	streamError    bool
	highestQuality bool
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func New(token, publicURL, allowedHosts string) http.Handler {
	return newServer(token, publicURL, splitHosts(allowedHosts))
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += int64(n)
	return n, err
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func newServer(token, publicURL string, mediaHosts []string) *server {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &server{
		token:           token,
		publicURL:       strings.TrimRight(publicURL, "/"),
		mediaHosts:      normalizeHosts(mediaHosts),
		logger:          log.Default(),
		now:             time.Now,
		wbiFetchTimeout: 3 * time.Second,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
					return dialer.DialContext(ctx, "tcp4", address)
				},
				DisableCompression:    true,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				MaxConnsPerHost:       100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 20 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	meta := &requestLog{route: requestRoute(r.URL.Path)}
	r = r.WithContext(context.WithValue(r.Context(), requestLogKey{}, meta))
	loggedWriter := &loggingResponseWriter{ResponseWriter: w}
	defer func() {
		status := loggedWriter.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Printf("request method=%s route=%s target_host=%q status=%d upstream_status=%d bytes=%d duration_ms=%d range=%t stream_error=%t highest_quality=%t remote=%q",
			r.Method, meta.route, meta.targetHost, status, meta.upstreamStatus, loggedWriter.bytes,
			time.Since(started).Milliseconds(), r.Header.Get("Range") != "", meta.streamError, meta.highestQuality, clientIP(r))
	}()
	w = loggedWriter

	setCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch {
	case r.URL.Path == "/":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Bili CF Acc is running")
	case strings.HasPrefix(r.URL.Path, "/proxy/"):
		s.handleMedia(w, r)
	case strings.HasPrefix(r.URL.Path, "/playurl/"):
		s.handlePlayurl(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleMedia(w http.ResponseWriter, r *http.Request) {
	target, err := s.targetFromRequest(r, "/proxy/")
	if err != nil {
		writeTargetError(w, err)
		return
	}
	meta := requestLogFrom(r)
	meta.targetHost = target.Hostname()
	if !allowedHost(target.Hostname(), s.mediaHosts) {
		http.Error(w, "Host not allowed", http.StatusForbidden)
		return
	}

	headers := copyRequestHeaders(r.Header, []string{
		"Accept", "If-Modified-Since", "If-None-Match", "If-Range", "Range", "User-Agent",
	})
	if isPlaylist("", target.Path) {
		headers.Del("If-Range")
		headers.Del("Range")
	}
	headers.Set("Referer", "https://www.bilibili.com/")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	response, finalURL, err := s.fetchAllowed(ctx, r.Method, target, headers, func(u *url.URL) bool {
		return allowedHost(u.Hostname(), s.mediaHosts)
	})
	if err != nil {
		http.Error(w, "Upstream request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	meta.upstreamStatus = response.StatusCode

	playlist := r.Method == http.MethodGet && isPlaylist(response.Header.Get("Content-Type"), finalURL.Path)
	if playlist && response.StatusCode == http.StatusPartialContent {
		http.Error(w, "Partial playlist not supported", http.StatusBadGateway)
		return
	}
	if playlist && response.StatusCode == http.StatusOK {
		timer := time.AfterFunc(30*time.Second, cancel)
		defer timer.Stop()
		body, err := io.ReadAll(io.LimitReader(response.Body, maxPlaylistSize+1))
		if err != nil || len(body) > maxPlaylistSize {
			http.Error(w, "Invalid playlist", http.StatusBadGateway)
			return
		}
		rewritten, err := s.rewritePlaylist(string(body), finalURL, s.publicBase(r))
		if err != nil {
			http.Error(w, "Invalid playlist URL", http.StatusBadGateway)
			return
		}
		copyResponseHeaders(w.Header(), response.Header)
		for _, name := range []string{"Accept-Ranges", "Content-Encoding", "Content-Length", "Content-Md5", "Content-Range", "Digest", "Etag", "Last-Modified"} {
			w.Header().Del(name)
		}
		w.WriteHeader(http.StatusOK)
		s.copyBody(w, strings.NewReader(rewritten), meta)
		return
	}

	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		s.copyBody(w, response.Body, meta)
	}
}

func (s *server) handlePlayurl(w http.ResponseWriter, r *http.Request) {
	target, err := s.targetFromRequest(r, "/playurl/")
	if err != nil {
		writeTargetError(w, err)
		return
	}
	meta := requestLogFrom(r)
	meta.targetHost = target.Hostname()
	if !allowedPlayurl(target) {
		http.Error(w, "Playurl API not allowed", http.StatusForbidden)
		return
	}

	headers := copyRequestHeaders(r.Header, []string{"Accept", "Accept-Language", "User-Agent"})
	if cookie := r.Header.Get("X-Bili-Cookie"); cookie != "" && !strings.ContainsAny(cookie, "\r\n") {
		headers.Set("Cookie", cookie)
	}
	referer := r.Header.Get("X-Bili-Referer")
	if referer == "" {
		referer = "https://www.bilibili.com/"
	}
	headers.Set("Referer", referer)
	if parsed, err := url.Parse(referer); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		headers.Set("Origin", parsed.Scheme+"://"+parsed.Host)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	qualityCtx, qualityCancel := context.WithTimeout(ctx, s.wbiFetchTimeout)
	upgradedTarget, upgraded, upgradeErr := s.highestQualityTarget(qualityCtx, target, headers)
	qualityCancel()
	if upgradeErr != nil {
		s.logger.Printf("quality_upgrade_failed target_host=%q", target.Hostname())
	} else if upgraded {
		target = upgradedTarget
		meta.highestQuality = true
	}
	response, _, err := s.fetchAllowed(ctx, r.Method, target, headers, allowedPlayurl)
	if err != nil {
		http.Error(w, "Upstream request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	meta.upstreamStatus = response.StatusCode
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		s.copyBody(w, response.Body, meta)
	}
}

func (s *server) copyBody(w http.ResponseWriter, body io.Reader, meta *requestLog) {
	buffer := copyBufferPool.Get().(*[]byte)
	defer copyBufferPool.Put(buffer)
	if _, err := io.CopyBuffer(w, body, *buffer); err != nil {
		meta.streamError = true
		panic(http.ErrAbortHandler)
	}
}
