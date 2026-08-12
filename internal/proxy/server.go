package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxPlaylistSize            = 4 << 20
	maxGRPCRequestSize         = 2 << 20
	maxGRPCResponseSize        = 8 << 20
	copyBufferSize             = 32 << 10
	grpcTunnelContentType      = "application/x-bili-acc-grpc"
	grpcOriginalContentTypeHdr = "X-Bili-Acc-Grpc-Content-Type"
	grpcTunnelStatusHdr        = "X-Bili-Acc-Grpc-Status"
)

var copyBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, copyBufferSize)
		return &buffer
	},
}

type server struct {
	token              string
	publicURL          string
	mediaHosts         []string
	client             *http.Client
	logger             *slog.Logger
	metrics            *metricsStore
	now                func() time.Time
	logMediaSuccess    bool
	errorDedupInterval time.Duration
	clientIPMode       string
	requestSequence    atomic.Uint64
	dedupMu            sync.Mutex
	dedup              map[string]*dedupState
	wbiMu              sync.Mutex
	wbiMixinKey        string
	wbiKeyExpires      time.Time
	wbiRefresh         chan struct{}
	wbiFetchTimeout    time.Duration
	mediaGroupMu       sync.Mutex
	mediaGroups        map[string]mediaGroup
}

type requestLog struct {
	started                time.Time
	ctx                    context.Context
	requestID              string
	route                  string
	targetHost             string
	upstreamStatus         int
	upstreamHeaderMS       int64
	upstreamHeaderObserved bool
	streamResult           string
	qualityParams          string
	grpcStatus             string
	errorStage             string
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status  int
	bytes   int64
	onWrite func(int64)
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
	if w.onWrite != nil && n > 0 {
		w.onWrite(int64(n))
	}
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
	now := time.Now
	return &server{
		token:              token,
		publicURL:          strings.TrimRight(publicURL, "/"),
		mediaHosts:         normalizeHosts(mediaHosts),
		logger:             slog.Default(),
		metrics:            newMetricsStore(now()),
		now:                now,
		errorDedupInterval: 10 * time.Second,
		clientIPMode:       "masked",
		dedup:              make(map[string]*dedupState),
		wbiFetchTimeout:    3 * time.Second,
		mediaGroups:        make(map[string]mediaGroup),
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
	started := s.now()
	meta := &requestLog{
		started:   started,
		ctx:       r.Context(),
		requestID: nextRequestID(&s.requestSequence),
		route:     requestRoute(r.URL.Path),
	}
	r = r.WithContext(context.WithValue(r.Context(), requestLogKey{}, meta))
	loggedWriter := &loggingResponseWriter{ResponseWriter: w}
	defer s.completeRequest(r, loggedWriter, meta)
	w = loggedWriter

	setCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	grpcPlayurl := strings.HasPrefix(r.URL.Path, "/playurl-grpc/")
	mediaRegistration := strings.HasPrefix(r.URL.Path, "/media-groups/")
	if grpcPlayurl || mediaRegistration {
		if r.Method != http.MethodPost {
			meta.errorStage = "method"
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
	} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
		meta.errorStage = "method"
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch {
	case r.URL.Path == "/":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Bili CF Acc is running")
	case strings.HasPrefix(r.URL.Path, "/proxy-group/"):
		s.handleMediaGroup(w, r)
	case strings.HasPrefix(r.URL.Path, "/proxy/"):
		s.handleMedia(w, r)
	case mediaRegistration:
		s.handleMediaGroupRegistration(w, r)
	case grpcPlayurl:
		s.handleGRPCPlayurl(w, r)
	case strings.HasPrefix(r.URL.Path, "/playurl/"):
		s.handlePlayurl(w, r)
	default:
		meta.errorStage = "routing"
		http.NotFound(w, r)
	}
}

func (s *server) handleMedia(w http.ResponseWriter, r *http.Request) {
	target, err := s.targetFromRequest(r, "/proxy/")
	if err != nil {
		requestLogFrom(r).errorStage = targetErrorStage(err)
		writeTargetError(w, err)
		return
	}
	s.handleMediaTargets(w, r, []*url.URL{target})
}

func (s *server) handleMediaTargets(w http.ResponseWriter, r *http.Request, targets []*url.URL) {
	meta := requestLogFrom(r)
	if len(targets) == 0 || !allowedHost(targets[0].Hostname(), s.mediaHosts) {
		meta.errorStage = "target_validation"
		http.Error(w, "Host not allowed", http.StatusForbidden)
		return
	}
	meta.targetHost = diagnosticHost(targets[0].Hostname())
	headers := copyRequestHeaders(r.Header, []string{
		"Accept", "If-Modified-Since", "If-None-Match", "If-Range", "Range", "User-Agent",
	})
	if isPlaylist("", targets[0].Path) {
		headers.Del("If-Range")
		headers.Del("Range")
	}
	headers.Set("Referer", "https://www.bilibili.com/")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	fetchStarted := s.now()
	response, finalURL, err := s.fetchMediaCandidates(ctx, r.Method, targets, headers)
	if err != nil {
		if requestCancelled(r, err) {
			markRequestCancelled(meta, "upstream_wait")
			return
		}
		meta.errorStage = "upstream_fetch"
		http.Error(w, "Upstream request failed", http.StatusBadGateway)
		return
	}
	meta.upstreamHeaderMS = max(s.now().Sub(fetchStarted).Milliseconds(), 0)
	meta.upstreamHeaderObserved = true
	defer response.Body.Close()
	meta.targetHost = diagnosticHost(finalURL.Hostname())
	meta.upstreamStatus = response.StatusCode

	playlist := r.Method == http.MethodGet && isPlaylist(response.Header.Get("Content-Type"), finalURL.Path)
	if playlist && response.StatusCode == http.StatusPartialContent {
		meta.errorStage = "playlist"
		http.Error(w, "Partial playlist not supported", http.StatusBadGateway)
		return
	}
	if playlist && response.StatusCode == http.StatusOK {
		timer := time.AfterFunc(30*time.Second, cancel)
		defer timer.Stop()
		body, err := io.ReadAll(io.LimitReader(response.Body, maxPlaylistSize+1))
		if err != nil || len(body) > maxPlaylistSize {
			meta.errorStage = "playlist"
			http.Error(w, "Invalid playlist", http.StatusBadGateway)
			return
		}
		rewritten, err := s.rewritePlaylist(string(body), finalURL, s.publicBase(r))
		if err != nil {
			meta.errorStage = "playlist"
			http.Error(w, "Invalid playlist URL", http.StatusBadGateway)
			return
		}
		copyResponseHeaders(w.Header(), response.Header)
		for _, name := range []string{"Accept-Ranges", "Content-Encoding", "Content-Length", "Content-Md5", "Content-Range", "Digest", "Etag", "Last-Modified"} {
			w.Header().Del(name)
		}
		enableMediaByteMetrics(w, s)
		w.WriteHeader(http.StatusOK)
		s.copyBody(w, strings.NewReader(rewritten), meta)
		return
	}

	copyResponseHeaders(w.Header(), response.Header)
	if response.StatusCode >= 200 && response.StatusCode < 400 {
		enableMediaByteMetrics(w, s)
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		s.copyBody(w, response.Body, meta)
	}
}

func (s *server) handleGRPCPlayurl(w http.ResponseWriter, r *http.Request) {
	target, err := s.targetFromRequest(r, "/playurl-grpc/")
	if err != nil {
		requestLogFrom(r).errorStage = targetErrorStage(err)
		writeTargetError(w, err)
		return
	}
	meta := requestLogFrom(r)
	meta.targetHost = diagnosticHost(target.Hostname())
	if !allowedGRPCPlayurl(target) {
		meta.errorStage = "target_validation"
		http.Error(w, "gRPC playurl API not allowed", http.StatusForbidden)
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	upstreamContentType := contentType
	if contentType == grpcTunnelContentType {
		upstreamContentType = strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get(grpcOriginalContentTypeHdr), ";")[0]))
	}
	if upstreamContentType != "application/grpc" && upstreamContentType != "application/grpc+proto" {
		meta.errorStage = "request_body"
		http.Error(w, "Unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGRPCRequestSize+1))
	if err != nil {
		if requestCancelled(r, err) {
			markRequestCancelled(meta, "request_body")
			return
		}
		meta.errorStage = "request_body"
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if len(body) > maxGRPCRequestSize {
		meta.errorStage = "request_body"
		http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	headers := copyGRPCRequestHeaders(r.Header)
	normalizeGRPCMetadata(headers)
	body, requestNormalized := normalizePlayViewUniteRequest(body, headers.Get("Grpc-Encoding"))
	if requestNormalized {
		headers.Del("Grpc-Encoding")
	}
	headers.Del(grpcOriginalContentTypeHdr)
	headers.Set("Content-Type", upstreamContentType)
	headers.Set("Accept-Encoding", "identity")
	headers.Set("Grpc-Accept-Encoding", "identity")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	fetchStarted := s.now()
	response, _, err := s.fetchAllowedBody(ctx, r.Method, target, headers, body, allowedGRPCPlayurl)
	if err != nil {
		if requestCancelled(r, err) {
			markRequestCancelled(meta, "upstream_wait")
			return
		}
		meta.errorStage = "upstream_fetch"
		http.Error(w, "Upstream request failed", http.StatusBadGateway)
		return
	}
	meta.upstreamHeaderMS = max(s.now().Sub(fetchStarted).Milliseconds(), 0)
	meta.upstreamHeaderObserved = true
	defer response.Body.Close()
	meta.upstreamStatus = response.StatusCode
	if rawHeaderStatus, ok := headerValue(response.Header, "Grpc-Status"); ok {
		meta.grpcStatus = normalizeGRPCStatusForWire(rawHeaderStatus)
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxGRPCResponseSize+1))
	if err != nil {
		if requestCancelled(r, err) {
			if meta.grpcStatus != "" && meta.grpcStatus != "0" {
				meta.errorStage = "grpc_status"
				return
			}
			if meta.upstreamStatus >= http.StatusBadRequest {
				meta.errorStage = "upstream_response"
				return
			}
			markRequestCancelled(meta, "response_body")
			return
		}
		meta.errorStage = "response_body"
		http.Error(w, "Invalid upstream response", http.StatusBadGateway)
		return
	}
	if len(responseBody) > maxGRPCResponseSize {
		meta.errorStage = "response_body"
		http.Error(w, "Upstream response too large", http.StatusBadGateway)
		return
	}
	if rawTrailerStatus, ok := headerValue(response.Trailer, "Grpc-Status"); ok {
		meta.grpcStatus = normalizeGRPCStatusForWire(rawTrailerStatus)
	} else if meta.grpcStatus == "" {
		meta.grpcStatus = "2"
	}

	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Del("Content-Length")
	w.Header().Del(grpcTunnelStatusHdr)
	w.Header().Set(grpcTunnelStatusHdr, meta.grpcStatus)
	announcedTrailers := len(response.Trailer)
	for name := range response.Trailer {
		w.Header().Add("Trailer", name)
	}
	w.WriteHeader(response.StatusCode)
	s.copyBody(w, bytes.NewReader(responseBody), meta)
	if len(response.Trailer) == announcedTrailers {
		for name, values := range response.Trailer {
			w.Header()[name] = values
		}
		return
	}
	for name, values := range response.Trailer {
		w.Header()[http.TrailerPrefix+name] = values
	}
}

func (s *server) handlePlayurl(w http.ResponseWriter, r *http.Request) {
	target, err := s.targetFromRequest(r, "/playurl/")
	if err != nil {
		requestLogFrom(r).errorStage = targetErrorStage(err)
		writeTargetError(w, err)
		return
	}
	meta := requestLogFrom(r)
	meta.targetHost = diagnosticHost(target.Hostname())
	if !allowedPlayurl(target) {
		meta.errorStage = "target_validation"
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
		meta.qualityParams = "failed"
		meta.errorStage = "quality_upgrade"
	} else if upgraded {
		target = upgradedTarget
		meta.qualityParams = "upgraded"
	} else {
		meta.qualityParams = "unchanged"
	}
	fetchStarted := s.now()
	response, _, err := s.fetchAllowed(ctx, r.Method, target, headers, allowedPlayurl)
	if err != nil {
		if requestCancelled(r, err) {
			markRequestCancelled(meta, "upstream_wait")
			return
		}
		meta.errorStage = "upstream_fetch"
		http.Error(w, "Upstream request failed", http.StatusBadGateway)
		return
	}
	meta.upstreamHeaderMS = max(s.now().Sub(fetchStarted).Milliseconds(), 0)
	meta.upstreamHeaderObserved = true
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
	if meta.route == "media" {
		s.metrics.streamStarted()
		defer s.metrics.streamFinished()
	}
	if _, err := io.CopyBuffer(w, body, *buffer); err != nil {
		meta.errorStage = "response_stream"
		if meta.ctx != nil && errors.Is(meta.ctx.Err(), context.Canceled) {
			meta.streamResult = "client_cancelled"
		} else {
			meta.streamResult = "error"
		}
		panic(http.ErrAbortHandler)
	}
	meta.streamResult = "complete"
}

func targetErrorStage(err error) string {
	if err != nil && err.Error() == "unauthorized" {
		return "authorization"
	}
	return "target_validation"
}

func normalizeGRPCStatusForWire(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "2"
	}
	status, err := strconv.Atoi(value)
	if err != nil || status < 0 || status > 16 {
		return "2"
	}
	return strconv.Itoa(status)
}

func headerValue(headers http.Header, name string) (string, bool) {
	values, ok := headers[http.CanonicalHeaderKey(name)]
	if !ok {
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}
	return values[0], true
}

func requestCancelled(r *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled)
}

func markRequestCancelled(meta *requestLog, stage string) {
	meta.streamResult = "client_cancelled"
	meta.errorStage = stage
}

func enableMediaByteMetrics(w http.ResponseWriter, s *server) {
	if writer, ok := w.(*loggingResponseWriter); ok {
		writer.onWrite = func(count int64) { s.metrics.addMediaBytes(s.now(), count) }
	}
}
