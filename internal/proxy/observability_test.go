package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSuccessfulMediaIsSilentAndCountsAcceptedBodyBytes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "payload")
	}))
	defer upstream.Close()

	now := time.Unix(1_700_000_000, 0)
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return now }
	app.metrics = newMetricsStore(now)
	app.logger = testLogger(&logs)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://cdn.bilivideo.com/video.m4s"), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "payload" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("successful media log = %q", logs.String())
	}
	snapshot := app.metrics.snapshot(now)
	if snapshot.Windows["1m"].Requests != 1 || snapshot.Windows["1m"].Routes["media"] != 1 || snapshot.Windows["1m"].Bytes != int64(len("payload")) || snapshot.ThroughputBPS == 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestHealthAndOptionsAreSilentAndNotCounted(t *testing.T) {
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = testLogger(&logs)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodOptions, "/proxy/example", nil),
	} {
		app.ServeHTTP(httptest.NewRecorder(), request)
	}
	if logs.Len() != 0 || app.metrics.snapshot(app.now()).Windows["1m"].Requests != 0 {
		t.Fatalf("logs=%q snapshot=%+v", logs.String(), app.metrics.snapshot(app.now()))
	}
}

func TestUpstream403IsVisibleButErrorBodyDoesNotCountAsThroughput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "forbidden")
	}))
	defer upstream.Close()
	now := time.Unix(1_700_000_000, 0)
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return now }
	app.metrics = newMetricsStore(now)
	app.logger = testLogger(&logs)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://cdn.bilivideo.com/video.m4s"), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	logged := logs.String()
	if response.Code != http.StatusForbidden || !strings.Contains(logged, "result=upstream_rejected") || !strings.Contains(logged, "error_stage=upstream_response") || !strings.Contains(logged, "upstream_status=403") {
		t.Fatalf("status=%d log=%q", response.Code, logged)
	}
	snapshot := app.metrics.snapshot(now)
	if snapshot.Windows["1m"].Bytes != 0 || snapshot.ThroughputBPS != 0 {
		t.Fatalf("error body counted as throughput: %+v", snapshot)
	}
}

func TestRepeatedFailuresDeduplicateWithBoundedSafeKeys(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return now }
	app.logger = testLogger(&logs)
	app.errorDedupInterval = 10 * time.Second
	target := "https://not-allowed.example/video.m4s?secret=query-value"
	for _, advance := range []time.Duration{0, time.Second, 11 * time.Second} {
		now = base.Add(advance)
		request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, target), nil)
		request.Header.Set("Cookie", "SESSDATA=cookie-value")
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d", response.Code)
		}
	}
	logged := logs.String()
	if strings.Count(logged, "event=request_complete") != 2 || !strings.Contains(logged, "repeats_suppressed=1") {
		t.Fatalf("deduplicated logs = %q", logged)
	}
	for _, secret := range []string{testToken, "query-value", "cookie-value", "/video.m4s"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %q", secret, logged)
		}
	}
	key := failureDedupKey(requestObservation{Route: strings.Repeat("r", 5_000), TargetHost: strings.Repeat("h", 5_000), Result: strings.Repeat("x", 5_000), ErrorStage: strings.Repeat("s", 5_000)})
	if len(key) > maxDedupKeyBytes {
		t.Fatalf("dedup key length = %d", len(key))
	}
}

func TestFailureDedupSeparatesGRPCAndUpstreamStatuses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = testLogger(&logs)
	app.errorDedupInterval = time.Minute
	base := requestObservation{Time: now, Route: "playurl_grpc", Method: http.MethodPost, TargetHost: "grpc.biliapi.net", Status: 200, Result: "grpc_error", ErrorStage: "grpc_status"}
	for _, pair := range []struct {
		grpc     string
		upstream int
	}{{"14", 200}, {"13", 200}, {"14", 503}, {"14", 200}} {
		observation := base
		observation.GRPCStatus = pair.grpc
		observation.UpstreamStatus = pair.upstream
		app.logRequest(observation, "")
	}
	if count := strings.Count(logs.String(), "event=request_complete"); count != 3 {
		t.Fatalf("alternating status logs=%d: %q", count, logs.String())
	}
}

func TestJSONRequestLogIsStructuredAndRedacted(t *testing.T) {
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "https://blocked.example/video.m4s?deadline=secret-query"), nil)
	request.Header.Set("Authorization", "Bearer secret-authorization")
	app.ServeHTTP(httptest.NewRecorder(), request)
	line := bytes.TrimSpace(logs.Bytes())
	if !json.Valid(line) || !bytes.Contains(line, []byte(`"event":"request_complete"`)) || !bytes.Contains(line, []byte(`"result":"client_rejected"`)) {
		t.Fatalf("JSON log = %s", line)
	}
	for _, secret := range []string{testToken, "secret-query", "secret-authorization", "/video.m4s"} {
		if bytes.Contains(line, []byte(secret)) {
			t.Fatalf("JSON log leaked %q: %s", secret, line)
		}
	}
}

func TestTrafficSummaryTreatsCancellationAsThirdOutcome(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return now }
	app.logger = testLogger(&logs)
	app.metrics = newMetricsStore(now.Add(-time.Hour))
	app.logTrafficSummary(now, time.Minute)
	if logs.Len() != 0 {
		t.Fatalf("empty summary = %q", logs.String())
	}
	app.metrics.record(requestObservation{Time: now, Route: "media", Status: 403, UpstreamStatus: 403, Result: "upstream_rejected", UpstreamHeaderObserved: true, UpstreamHeaderMS: 18})
	app.metrics.record(requestObservation{Time: now, Route: "media", Status: 206, UpstreamStatus: 206, Result: "ok", UpstreamHeaderObserved: true, UpstreamHeaderMS: 42})
	app.metrics.record(requestObservation{Time: now, Route: "media", Result: "client_cancelled"})
	app.metrics.addMediaBytes(now, 1_170)
	app.logTrafficSummary(now, time.Minute)
	logged := logs.String()
	for _, expected := range []string{"event=traffic_summary", "requests=3", "success=1", "failed=1", "media_requests=3", "bytes=1170", "client_cancelled=1", "status_403=1", "upstream_header_p95_ms=50"} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("summary missing %q: %q", expected, logged)
		}
	}
	window := app.metrics.snapshot(now).Windows["1m"]
	if window.SuccessRate != 50 {
		t.Fatalf("success rate includes cancellation: %+v", window)
	}
}

func TestPreHeaderCancellationIsSilentWithoutSynthetic502(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return now }
	app.metrics = newMetricsStore(now)
	app.logger = testLogger(&logs)
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})

	tests := []struct {
		method      string
		url         string
		body        io.Reader
		contentType string
	}{
		{http.MethodGet, proxyPath("/proxy/", testToken, "https://cdn.bilivideo.com/video.m4s"), nil, ""},
		{http.MethodGet, proxyPath("/playurl/", testToken, "https://api.bilibili.com/x/player/playurl?cid=1"), nil, ""},
		{http.MethodPost, proxyPath("/playurl-grpc/", testToken, "https://grpc.biliapi.net"+grpcPlayurlPath), bytes.NewReader([]byte{0, 0, 0, 0, 0}), "application/grpc"},
	}
	for _, test := range tests {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request := httptest.NewRequest(test.method, test.url, test.body).WithContext(ctx)
		if test.contentType != "" {
			request.Header.Set("Content-Type", test.contentType)
		}
		writer := &trackingResponseWriter{header: make(http.Header)}
		app.ServeHTTP(writer, request)
		if writer.wroteHeader || writer.writes != 0 {
			t.Fatalf("cancellation wrote response for %s: %+v", test.url, writer)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("cancellation logs = %q", logs.String())
	}
	window := app.metrics.snapshot(now).Windows["1m"]
	if window.Requests != 3 || window.ClientCancelled != 3 || window.Success != 0 || window.Failed != 0 || window.SuccessRate != 0 || len(window.Statuses) != 0 || window.UpstreamHeaderP95MS != 0 {
		t.Fatalf("cancellation window = %+v", window)
	}
}

func TestMediaIdleTimeoutIsClassifiedAsUpstreamStall(t *testing.T) {
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = testLogger(&logs)
	app.mediaIdleTimeout = 10 * time.Millisecond
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Type": {"video/mp4"}},
			Body:       &blockingReadCloser{closed: make(chan struct{})},
			Request:    request,
		}, nil
	})
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "https://cdn.bilivideo.com/video.m4s"), nil)
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("panic=%v", recovered)
			}
		}()
		app.ServeHTTP(httptest.NewRecorder(), request)
	}()
	if !strings.Contains(logs.String(), "result=upstream_stall") || !strings.Contains(logs.String(), "error_stage=upstream_idle") {
		t.Fatalf("log=%q", logs.String())
	}
	snapshot := app.metrics.snapshot(app.now())
	if snapshot.Windows["15m"].UpstreamStalls != 1 || len(snapshot.Hosts) != 1 || snapshot.Hosts[0].UpstreamStalls != 1 {
		t.Fatalf("stall snapshot=%+v", snapshot)
	}
}

func TestUpstreamFailuresRemainFailuresWhenStreamingIsCancelled(t *testing.T) {
	tests := []struct {
		name          string
		route         string
		method        string
		target        string
		contentType   string
		status        int
		grpcStatus    string
		expected      string
		expectedStage string
		expectPanic   bool
	}{
		{"media", "/proxy/", http.MethodGet, "https://cdn.bilivideo.com/video.m4s", "", http.StatusForbidden, "", "upstream_rejected", "upstream_response", true},
		{"grpc", "/playurl-grpc/", http.MethodPost, "https://grpc.biliapi.net" + grpcPlayurlPath, "application/grpc", http.StatusOK, "14", "grpc_error", "grpc_status", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var logs bytes.Buffer
			app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
			app.logger = testLogger(&logs)
			app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				header := make(http.Header)
				header.Set("Content-Type", test.contentType)
				if test.grpcStatus != "" {
					header.Set("Grpc-Status", test.grpcStatus)
				}
				return &http.Response{StatusCode: test.status, Header: header, Body: &cancelOnReadCloser{cancel: cancel}, Request: request}, nil
			})
			var body io.Reader
			if test.route == "/playurl-grpc/" {
				body = bytes.NewReader([]byte{0, 0, 0, 0, 0})
			}
			request := httptest.NewRequest(test.method, proxyPath(test.route, testToken, test.target), body).WithContext(ctx)
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			func() {
				defer func() {
					recovered := recover()
					if test.expectPanic && recovered != http.ErrAbortHandler {
						t.Fatalf("panic = %v", recovered)
					}
					if !test.expectPanic && recovered != nil {
						t.Fatalf("unexpected panic = %v", recovered)
					}
				}()
				app.ServeHTTP(httptest.NewRecorder(), request)
			}()
			if !strings.Contains(logs.String(), " result="+test.expected+" ") || !strings.Contains(logs.String(), "error_stage="+test.expectedStage) || strings.Contains(logs.String(), " result=client_cancelled ") {
				t.Fatalf("log = %q", logs.String())
			}
		})
	}
}

func TestCandidateSelectionMetricsTrackRecoveryAndExhaustion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	metrics := newMetricsStore(now)
	metrics.recordCandidateSelection(now, 3, true, false)
	metrics.recordCandidateSelection(now, 2, false, true)
	window := metrics.snapshot(now).Windows["15m"]
	if window.CandidateAttempts != 5 || window.Fallbacks != 2 || window.FallbackRecoveries != 1 || window.CandidateExhausted != 1 {
		t.Fatalf("candidate metrics=%+v", window)
	}
}

func TestPlayurlQualityMetricsSeparateVideoAndLiveLabels(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	metrics := newMetricsStore(now)
	metrics.record(requestObservation{Time: now, Route: "playurl", Result: "ok", PlayurlKind: "video", ActualQuality: 116, QualityLabel: "1080P60"})
	metrics.record(requestObservation{Time: now, Route: "playurl", Result: "ok", PlayurlKind: "live", ActualQuality: 10000, QualityLabel: "原画 1080P60"})
	window := metrics.snapshot(now).Windows["15m"]
	if window.VideoQualities["1080P60"] != 1 || window.LiveQualities["原画 1080P60"] != 1 {
		t.Fatalf("quality metrics=%+v", window)
	}
}

func TestFailedUpstreamAttemptDoesNotRecordHeaderLatency(t *testing.T) {
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = testLogger(&logs)
	app.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial https://api.bilibili.com/path?secret=query-value failed")
	})
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, "https://api.bilibili.com/x/player/playurl?cid=secret-query"), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(logs.String(), "result=upstream_error") || strings.Contains(logs.String(), "upstream_header_ms=") || app.metrics.snapshot(app.now()).Windows["1m"].UpstreamHeaderP95MS != 0 {
		t.Fatalf("status=%d log=%q snapshot=%+v", response.Code, logs.String(), app.metrics.snapshot(app.now()))
	}
}

func TestHTTP2GRPCTrailersPropagateClassifyAndStayPrivate(t *testing.T) {
	secretMessage := "account-secret-message"
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Fatalf("protocol = %q", r.Proto)
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set(grpcTunnelStatusHdr, "7")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0, 0, 0, 0, 0})
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "14")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", secretMessage)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = testLogger(&logs)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodPost, proxyPath("/playurl-grpc/", testToken, "https://grpc.biliapi.net"+grpcPlayurlPath), bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	request.Header.Set("Content-Type", grpcTunnelContentType)
	request.Header.Set(grpcOriginalContentTypeHdr, "application/grpc")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	result := response.Result()
	defer result.Body.Close()
	_, _ = io.ReadAll(result.Body)
	if result.Header.Get(grpcTunnelStatusHdr) != "14" {
		t.Fatalf("tunneled status header = %q", result.Header.Get(grpcTunnelStatusHdr))
	}
	if result.Trailer.Get("Grpc-Status") != "14" || result.Trailer.Get("Grpc-Message") != secretMessage {
		t.Fatalf("trailers = %v", result.Trailer)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "result=grpc_error") || !strings.Contains(logs.String(), "grpc_status=14") || strings.Contains(logs.String(), secretMessage) {
		t.Fatalf("log = %q", logs.String())
	}
	snapshot := app.metrics.snapshot(app.now())
	if snapshot.GRPCStatuses["14"] != 1 || len(snapshot.RecentErrors) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	encoded, _ := json.Marshal(snapshot)
	if bytes.Contains(encoded, []byte(secretMessage)) {
		t.Fatalf("snapshot leaked grpc-message: %s", encoded)
	}
}

func TestFixedTimeBucketsDoNotUndercountAndRemainBounded(t *testing.T) {
	now := time.Unix(1_700_000_899, 0)
	metrics := newMetricsStore(now.Add(-time.Hour))
	for second := 0; second < metricWindowSeconds; second++ {
		timestamp := now.Add(-time.Duration(metricWindowSeconds-1-second) * time.Second)
		metrics.addMediaBytes(timestamp, 1_000)
		for index := 0; index < 100; index++ {
			observation := requestObservation{Time: timestamp, Route: "media", TargetHost: fmt.Sprintf("h%d.cdn.bilivideo.com", index), Status: 206, UpstreamStatus: 206, Result: "ok", UpstreamHeaderObserved: true, UpstreamHeaderMS: 10}
			switch {
			case index < 5:
				observation.Status = 403
				observation.UpstreamStatus = 403
				observation.Result = "upstream_rejected"
				observation.UpstreamHeaderMS = 200
			case index < 10:
				observation.Status = 0
				observation.UpstreamStatus = 0
				observation.Result = "client_cancelled"
				observation.UpstreamHeaderObserved = false
			}
			metrics.record(observation)
		}
	}
	snapshot := metrics.snapshot(now)
	one := snapshot.Windows["1m"]
	fifteen := snapshot.Windows["15m"]
	if one.Requests != 6_000 || one.Success != 5_400 || one.Failed != 300 || one.ClientCancelled != 300 || one.Bytes != 60_000 {
		t.Fatalf("1m window = %+v", one)
	}
	if fifteen.Requests != 90_000 || fifteen.Success != 81_000 || fifteen.Failed != 4_500 || fifteen.ClientCancelled != 4_500 || fifteen.Bytes != 900_000 {
		t.Fatalf("15m window = %+v", fifteen)
	}
	if fifteen.SuccessRate < 94.7 || fifteen.SuccessRate > 94.8 || fifteen.UpstreamHeaderP50MS != 10 || fifteen.UpstreamHeaderP95MS != 200 {
		t.Fatalf("15m quality = %+v", fifteen)
	}
	foundOverflow := false
	for _, host := range snapshot.Hosts {
		if host.Host == overflowHost {
			foundOverflow = true
		}
	}
	if !foundOverflow || len(snapshot.RecentErrors) != maxRecentErrors || snapshot.MetricsRetentionSeconds != metricWindowSeconds {
		t.Fatalf("bounded snapshot = %+v", snapshot)
	}
	metrics.mu.Lock()
	for _, bucket := range metrics.buckets {
		if len(bucket.Hosts) > maxHostsPerBucket {
			metrics.mu.Unlock()
			t.Fatalf("host cardinality = %d", len(bucket.Hosts))
		}
	}
	metrics.mu.Unlock()
}

func TestHostValidationRejectsOversizedAndMalformedNames(t *testing.T) {
	validMaximum := strings.Join([]string{strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("c", 63), strings.Repeat("d", 61)}, ".")
	if len(validMaximum) != 253 || !validDNSHostname(validMaximum) {
		t.Fatalf("valid maximum rejected: %d", len(validMaximum))
	}
	invalidHosts := []string{
		strings.Repeat("a", 64) + ".bilivideo.com",
		validMaximum + "x",
		"bad_name.bilivideo.com",
		"-bad.bilivideo.com",
		"bad-.bilivideo.com",
	}
	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = testLogger(&logs)
	app.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid host reached upstream")
		return nil, nil
	})
	for _, host := range invalidHosts {
		request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "https://"+host+"/video.m4s"), nil)
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("host %q status=%d", host, response.Code)
		}
	}
	if strings.Contains(logs.String(), strings.Repeat("a", 64)) || strings.Contains(logs.String(), "bad_name") {
		t.Fatalf("hostile hostname retained in logs: %q", logs.String())
	}
	if normalized := normalizeHosts(append([]string{"bilivideo.com"}, invalidHosts...)); len(normalized) != 1 || normalized[0] != "bilivideo.com" {
		t.Fatalf("normalized hosts = %v", normalized)
	}
}

func TestClientIPLoggingModesValidateValues(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Forwarded-For", "153.242.102.55")
	if got := clientIPForLog(request, "full"); got != "153.242.102.55" {
		t.Fatalf("full IP = %q", got)
	}
	if got := clientIPForLog(request, "masked"); got != "153.242.102.x" {
		t.Fatalf("masked IP = %q", got)
	}
	request.Header.Set("X-Forwarded-For", "attacker-controlled-text")
	if got := clientIPForLog(request, "full"); got != "unknown" {
		t.Fatalf("invalid full IP = %q", got)
	}
	if got := clientIPForLog(request, "off"); got != "" {
		t.Fatalf("off IP = %q", got)
	}
	if got := normalizeClientIPMode(""); got != "masked" {
		t.Fatalf("default mode = %q", got)
	}
}

func TestDiagnosticsHandlerHTMLAPIAndSecurityHeaders(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	handler := (&Application{server: app}).Handler()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/diagnostics/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%q", page.Code, page.Body.String())
	}
	for _, expected := range []string{"Signal Room", "Active streams", "Playback stalls", "15m traffic", "Fallback health", "LIVE EDGE ACTIVE", "latest 3 complete segments", "Playurl quality", "JSON responses", "Video", "Live", "reported current quality", "video_qualities", "live_qualities", "Request mix", "Rolling windows", "CDN and upstream hosts", "Stalls", "gRPC trailers", "Recent sanitized errors", "rel=\"icon\" href=\"data:,\"", "scope=\"col\"", "aria-live=\"polite\"", "data-label", "HTTP / upstream", "Result / gRPC", "@media (max-width: 560px)"} {
		if !strings.Contains(page.Body.String(), expected) {
			t.Fatalf("diagnostics page missing %q", expected)
		}
	}
	assertDiagnosticsHeaders(t, page.Header())
	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/diagnostics/api/snapshot", nil))
	if api.Code != http.StatusOK || !strings.Contains(api.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("api status=%d headers=%v", api.Code, api.Header())
	}
	assertDiagnosticsHeaders(t, api.Header())
	var snapshot diagnosticsSnapshot
	if err := json.Unmarshal(api.Body.Bytes(), &snapshot); err != nil || snapshot.MetricsRetentionSeconds != metricWindowSeconds {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if !strings.Contains(api.Body.String(), `"recent_errors":[]`) {
		t.Fatalf("empty recent errors must be an array: %s", api.Body.String())
	}
	method := httptest.NewRecorder()
	handler.ServeHTTP(method, httptest.NewRequest(http.MethodPost, "/diagnostics/api/snapshot", nil))
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("method status=%d headers=%v", method.Code, method.Header())
	}
	if got := app.metrics.window(app.now(), time.Minute).Requests; got != 0 {
		t.Fatalf("diagnostics requests entered proxy metrics: %d", got)
	}
}

func assertDiagnosticsHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	for _, name := range []string{"Cache-Control", "Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if headers.Get(name) == "" {
			t.Fatalf("missing %s: %v", name, headers)
		}
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", headers.Get("Cache-Control"))
	}
}

type trackingResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	writes      int
}

func (w *trackingResponseWriter) Header() http.Header { return w.header }
func (w *trackingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.wroteHeader = true
}
func (w *trackingResponseWriter) Write(body []byte) (int, error) {
	w.writes++
	return len(body), nil
}

type cancelOnReadCloser struct {
	cancel context.CancelFunc
}

func (r *cancelOnReadCloser) Read([]byte) (int, error) {
	r.cancel()
	return 0, context.Canceled
}
func (*cancelOnReadCloser) Close() error { return nil }
