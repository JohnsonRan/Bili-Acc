package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

const testToken = "test-token"

func proxyPath(prefix, token, target string) string {
	u, _ := url.Parse(target)
	origin := u.Scheme + "://" + u.Host
	return prefix + token + "/" + base64.RawURLEncoding.EncodeToString([]byte(origin)) + u.RequestURI()
}

func TestMediaProxyForwardsRange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-0" {
			t.Fatalf("Range = %q", got)
		}
		w.Header().Set("Content-Range", "bytes 0-0/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://cdn.bilivideo.com/video.m4s"), nil)
	request.Header.Set("Range", "bytes=0-0")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent || response.Header().Get("Content-Range") != "bytes 0-0/10" || response.Body.String() != "x" {
		t.Fatalf("status=%d range=%q body=%q", response.Code, response.Header().Get("Content-Range"), response.Body.String())
	}
}

func TestPlayurlProxyForwardsCookie(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Host; got != "api.bilibili.com" {
			t.Fatalf("Host = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "SESSDATA=secret" {
			t.Fatalf("Cookie = %q", got)
		}
		if got := r.Header.Get("Referer"); got != "https://www.bilibili.com/video/BV1" {
			t.Fatalf("Referer = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)

	target := "https://api.bilibili.com/x/player/playurl?bvid=BV1&cid=1"
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, target), nil)
	request.Header.Set("X-Bili-Cookie", "SESSDATA=secret")
	request.Header.Set("X-Bili-Referer", "https://www.bilibili.com/video/BV1")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != `{"code":0}` {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestGRPCPlayurlProxyForwardsPOSTBodyMetadataAndTrailers(t *testing.T) {
	requestBody := []byte{0, 0, 0, 0, 2, 8, 1}
	responseBody := []byte{0, 0, 0, 0, 2, 8, 2}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Fatalf("protocol = %q", r.Proto)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q", r.Method)
		}
		if r.Host != "grpc.biliapi.net" || r.URL.Path != grpcPlayurlPath {
			t.Fatalf("target = %q%q", r.Host, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, requestBody) {
			t.Fatalf("body = %v err=%v", body, err)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Fatalf("Accept-Encoding = %q", got)
		}
		if got := r.Header.Get("Grpc-Accept-Encoding"); got != "identity" {
			t.Fatalf("Grpc-Accept-Encoding = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "identify_v1 secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Bili-Device"); got != "device-metadata" {
			t.Fatalf("X-Bili-Device = %q", got)
		}
		for _, name := range []string{"X-Bili-Cookie", "X-Internal", "Proxy-Connection", "Forwarded", "X-Forwarded-For", "X-Real-Ip", "Cf-Connecting-Ip"} {
			if r.Header.Get(name) != "" {
				t.Fatalf("proxy header %s leaked: %v", name, r.Header)
			}
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Content-Length", "7")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
		w.Header().Set(http.TrailerPrefix+"X-Rpc-Metadata", "complete")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	target := "https://grpc.biliapi.net" + grpcPlayurlPath
	request := httptest.NewRequest(http.MethodPost, proxyPath("/playurl-grpc/", testToken, target), bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/grpc")
	request.Header.Set("Grpc-Accept-Encoding", "gzip,identity")
	request.Header.Set("Authorization", "identify_v1 secret")
	request.Header.Set("X-Bili-Device", "device-metadata")
	request.Header.Set("X-Bili-Cookie", "must-not-forward")
	request.Header.Set("Connection", "X-Internal")
	request.Header.Set("Proxy-Connection", "keep-alive")
	request.Header.Set("X-Internal", "must-not-forward")
	request.Header.Set("Forwarded", "for=153.242.102.0")
	request.Header.Set("X-Forwarded-For", "153.242.102.0")
	request.Header.Set("X-Real-IP", "153.242.102.0")
	request.Header.Set("CF-Connecting-IP", "153.242.102.0")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)

	result := response.Result()
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	if err != nil || result.StatusCode != http.StatusOK || !bytes.Equal(body, responseBody) {
		t.Fatalf("status=%d body=%v err=%v", result.StatusCode, body, err)
	}
	if result.Header.Get("Content-Type") != "application/grpc" || result.Header.Get("Content-Length") != "" {
		t.Fatalf("headers=%v", result.Header)
	}
	if result.Trailer.Get("Grpc-Status") != "0" || result.Trailer.Get("X-Rpc-Metadata") != "complete" {
		t.Fatalf("trailers=%v", result.Trailer)
	}
}

func TestGRPCPlayurlProxyRejectsOtherMethodsAndEndpoints(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	target := "https://grpc.biliapi.net" + grpcPlayurlPath
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl-grpc/", testToken, target), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, proxyPath("/playurl-grpc/", testToken, "https://grpc.biliapi.net/other.Service/Call"), strings.NewReader("body"))
	request.Header.Set("Content-Type", "application/grpc")
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("other endpoint status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, proxyPath("/playurl-grpc/", testToken, "https://evil.grpc.biliapi.net"+grpcPlayurlPath), strings.NewReader("body"))
	request.Header.Set("Content-Type", "application/grpc")
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("subdomain status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, proxyPath("/playurl-grpc/", testToken, target), strings.NewReader("body"))
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, proxyPath("/proxy/", testToken, "https://cdn.bilivideo.com/video.m4s"), strings.NewReader("body"))
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("media POST status = %d", response.Code)
	}
}

func TestGRPCPlayurlProxyEnforcesRequestSizeLimit(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	var upstreamRequests atomic.Int32
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstreamRequests.Add(1)
		body, err := io.ReadAll(request.Body)
		if err != nil || len(body) != maxGRPCRequestSize {
			t.Fatalf("upstream body size=%d err=%v", len(body), err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/grpc"}},
			Body:       io.NopCloser(bytes.NewReader([]byte{0, 0, 0, 0, 0})),
			Request:    request,
		}, nil
	})
	target := "https://grpc.biliapi.net" + grpcPlayurlPath
	path := proxyPath("/playurl-grpc/", testToken, target)

	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(make([]byte, maxGRPCRequestSize)))
	request.Header.Set("Content-Type", "application/grpc")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK || upstreamRequests.Load() != 1 {
		t.Fatalf("exact limit status=%d upstream=%d", response.Code, upstreamRequests.Load())
	}

	request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(make([]byte, maxGRPCRequestSize+1)))
	request.Header.Set("Content-Type", "application/grpc")
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || upstreamRequests.Load() != 1 {
		t.Fatalf("over limit status=%d upstream=%d", response.Code, upstreamRequests.Load())
	}

	request = httptest.NewRequest(http.MethodPost, path, nil)
	request.Header.Set("Content-Type", "application/grpc")
	request.Body = errorReadCloser{}
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || upstreamRequests.Load() != 1 {
		t.Fatalf("read error status=%d upstream=%d", response.Code, upstreamRequests.Load())
	}
}

func TestPlayurlRequestsHighestQualityWithoutChangingResponseMetadata(t *testing.T) {
	upstreamBody := `{"code":0,"data":{"quality":80,"accept_quality":[127,120,80]}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		for name, expected := range map[string]string{"qn": "127", "fnver": "0", "fnval": "4048", "fourk": "1"} {
			if got := query.Get(name); got != expected {
				t.Errorf("%s = %q", name, got)
			}
		}
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	var logs bytes.Buffer
	app.logger = testLogger(&logs)
	target := "https://api.bilibili.com/x/player/playurl?bvid=BV1&cid=1&qn=80"
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, target), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != upstreamBody {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if got := logs.String(); !strings.Contains(got, "route=playurl") || !strings.Contains(got, "quality_params=upgraded") || strings.Contains(got, "range=") || strings.Contains(got, "highest_quality=") {
		t.Fatalf("playurl log = %q", got)
	}
}

func TestWBIPlayurlQualityUpgradeRefreshesAndCachesSignature(t *testing.T) {
	var navRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/web-interface/nav":
			navRequests++
			if got := r.Header.Get("Cookie"); got != "SESSDATA=secret" {
				t.Errorf("nav Cookie = %q", got)
			}
			_, _ = io.WriteString(w, `{"data":{"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png","sub_url":"https://i0.hdslb.com/bfs/wbi/4932caff0ff746eab6f01bf08b70ac45.png"}}}`)
		case "/x/player/wbi/playurl":
			query := r.URL.Query()
			if query.Get("qn") != "127" || query.Get("fnval") != "4048" || query.Get("fourk") != "1" {
				t.Errorf("quality query = %q", r.URL.RawQuery)
			}
			if query.Get("wts") != "1702204169" || query.Get("w_rid") != "8c4e938a1620d52d755a44c5e7262549" {
				t.Errorf("WBI query = %q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"code":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return time.Unix(1702204169, 0) }
	app.client.Transport = transportTo(upstream)
	target := "https://api.bilibili.com/x/player/wbi/playurl?bvid=BV1&cid=1&qn=80&w_rid=old&wts=1"
	for range 2 {
		request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, target), nil)
		request.Header.Set("X-Bili-Cookie", "SESSDATA=secret")
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%q", response.Code, response.Body.String())
		}
	}
	if navRequests != 1 {
		t.Fatalf("nav requests = %d", navRequests)
	}
}

func TestWBIRefreshFailureFallsBackToOriginalRequest(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	var logs bytes.Buffer
	app.logger = testLogger(&logs)
	app.wbiFetchTimeout = 20 * time.Millisecond
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/x/web-interface/nav":
			<-request.Context().Done()
			return nil, request.Context().Err()
		case "/x/player/wbi/playurl":
			query := request.URL.Query()
			if query.Get("qn") != "80" || query.Get("w_rid") != "old" || query.Get("wts") != "1" {
				t.Errorf("fallback query = %q", request.URL.RawQuery)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":0}`)),
				Request:    request,
			}, nil
		default:
			return nil, errors.New("unexpected request")
		}
	})
	target := "https://api.bilibili.com/x/player/wbi/playurl?bvid=BV1&cid=1&qn=80&w_rid=old&wts=1"
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, target), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"code":0}` {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if got := logs.String(); strings.Count(got, "event=request_complete") != 1 || !strings.Contains(got, "result=degraded") || !strings.Contains(got, "error_stage=quality_upgrade") || !strings.Contains(got, "quality_params=failed") {
		t.Fatalf("failed quality log = %q", got)
	}
}

func TestWBIRefreshIsSharedAcrossConcurrentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var navRequests atomic.Int32
		release := make(chan struct{})
		app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
		app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			navRequests.Add(1)
			<-release
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"data":{"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png","sub_url":"https://i0.hdslb.com/bfs/wbi/4932caff0ff746eab6f01bf08b70ac45.png"}}}`)),
				Request:    request,
			}, nil
		})

		results := make(chan error, 2)
		var requests sync.WaitGroup
		for range 2 {
			requests.Go(func() {
				_, err := app.getWBIKey(context.Background(), nil)
				results <- err
			})
		}

		synctest.Wait()
		if got := navRequests.Load(); got != 1 {
			t.Fatalf("nav requests before release = %d", got)
		}
		close(release)
		requests.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestWBIRejectsMalformedKeys(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":{"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/not-hex.png","sub_url":"https://i0.hdslb.com/bfs/wbi/4932caff0ff746eab6f01bf08b70ac45.png"}}}`)),
			Request:    request,
		}, nil
	})
	if _, err := app.getWBIKey(context.Background(), nil); err == nil {
		t.Fatal("malformed WBI key was accepted")
	}
	if app.wbiMixinKey != "" {
		t.Fatal("malformed WBI key was cached")
	}
}

func TestSignWBIKnownVector(t *testing.T) {
	values := url.Values{"foo": {"114"}, "bar": {"514"}, "zab": {"1919810"}}
	signed := signWBI(values, "ea1db124af3c7062474693fa704f4ff8", 1702204169)
	if got := signed.Get("w_rid"); got != "8f6f2b5b3d485fe1886cec6a0be8c5d4" {
		t.Fatalf("w_rid = %q", got)
	}
	if got := signed.Get("wts"); got != "1702204169" {
		t.Fatalf("wts = %q", got)
	}
	encoded := encodeQuery(signWBI(url.Values{"space": {"one two"}, "filtered": {"!'()*"}}, "ea1db124af3c7062474693fa704f4ff8", 1702204169))
	if !strings.Contains(encoded, "space=one%20two") || !strings.Contains(encoded, "filtered=") || strings.ContainsAny(encoded, "!'()*") {
		t.Fatalf("encoded query = %q", encoded)
	}
}

func TestLivePlayurlRequestsHighestQuality(t *testing.T) {
	tests := []struct {
		path  string
		name  string
		value string
	}{
		{"/xlive/web-room/v2/index/getRoomPlayInfo", "qn", "10000"},
		{"/room/v1/Room/playUrl", "quality", "4"},
	}
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	for _, test := range tests {
		target, _ := url.Parse("https://api.live.bilibili.com" + test.path + "?room_id=1")
		upgraded, ok, err := app.highestQualityTarget(context.Background(), target, nil)
		if err != nil || !ok || upgraded.Query().Get(test.name) != test.value {
			t.Fatalf("path=%s upgraded=%v ok=%t err=%v", test.path, upgraded, ok, err)
		}
	}
}

func TestTransportUsesControlledIPv4Egress(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	transport := app.client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("environment proxy must be disabled")
	}
	if transport.DialContext == nil || !transport.ForceAttemptHTTP2 {
		t.Fatal("controlled dialer and HTTP/2 must be enabled")
	}
	if transport.TLSHandshakeTimeout == 0 || transport.ResponseHeaderTimeout == 0 || transport.MaxConnsPerHost == 0 {
		t.Fatal("transport resource limits are incomplete")
	}
}

func TestProxyRejectsOriginsWithCustomPorts(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", []string{"bilivideo.com"})
	target := "https://cdn.bilivideo.com:8443/video.m4s"
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, target), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestPlayurlProxyRejectsOtherAPIs(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	target := "https://api.bilibili.com/x/web-interface/nav"
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, target), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestPlaylistRangeIsRemovedAndPartialResponseRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "" {
			t.Fatalf("Range = %q", got)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "segment.ts")
	}))
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://live.bilivideo.com/index.m3u8"), nil)
	request.Header.Set("Range", "bytes=0-100")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestUpstreamCannotOverrideCORS(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://evil.example")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://cdn.bilivideo.com/video.m4s"), nil)
	request.Header.Set("Origin", "https://www.bilibili.com")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if got := response.Header().Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != "https://www.bilibili.com" {
		t.Fatalf("CORS = %q", got)
	}
}

func TestRequestLogIsUsefulAndRedactsSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "x")
	}))
	defer upstream.Close()

	var output bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = testLogger(&output)
	app.logMediaSuccess = true
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://cdn.bilivideo.com/video.m4s?secret=query-value"), nil)
	request.Header.Set("Cookie", "SESSDATA=cookie-value")
	request.Header.Set("Range", "bytes=0-0")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)

	logged := output.String()
	for _, expected := range []string{"event=request_complete", "route=media", "target_host=cdn.bilivideo.com", "status=206", "upstream_status=206", "result=ok", "bytes=1", "range=true", "stream_result=complete"} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("log missing %q: %q", expected, logged)
		}
	}
	for _, irrelevant := range []string{"quality_params=", "highest_quality="} {
		if strings.Contains(logged, irrelevant) {
			t.Fatalf("media log contains %q: %q", irrelevant, logged)
		}
	}
	for _, secret := range []string{testToken, "query-value", "cookie-value"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %q", secret, logged)
		}
	}
}

func TestRedirectBodyDrainHasDeadline(t *testing.T) {
	body := &blockingReadCloser{closed: make(chan struct{})}
	response := &http.Response{Body: body, ContentLength: 1}
	started := time.Now()
	closeRedirectBody(response)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("redirect drain took %s", elapsed)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("redirect body was not closed")
	}
}

type errorReadCloser struct{}

func (errorReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (errorReadCloser) Close() error {
	return nil
}

type blockingReadCloser struct {
	closed chan struct{}
}

func (b *blockingReadCloser) Read([]byte) (int, error) {
	<-b.closed
	return 0, errors.New("closed")
}

func (b *blockingReadCloser) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestPlaylistRewrite(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", []string{"bilivideo.com"})
	base, _ := url.Parse("https://live.bilivideo.com/path/index.m3u8")
	body := "#EXTM3U\n \t\r\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\nsegment.ts"
	rewritten, err := app.rewritePlaylist(body, base, "https://proxy.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/path/key.bin", "/path/segment.ts"} {
		if !strings.Contains(rewritten, suffix) {
			t.Fatalf("missing %q in %q", suffix, rewritten)
		}
	}
	if !strings.Contains(rewritten, "\n \t\r\n") {
		t.Fatalf("whitespace-only line was changed: %q", rewritten)
	}
}

func TestCopyResponseHeadersDropsConnectionExtensions(t *testing.T) {
	source := make(http.Header)
	source.Set("Connection", "X-Internal")
	source.Set("Proxy-Connection", "keep-alive")
	source.Set("X-Internal", "secret")
	source.Set("Content-Type", "video/mp4")
	destination := make(http.Header)
	copyResponseHeaders(destination, source)
	if destination.Get("X-Internal") != "" || destination.Get("Connection") != "" || destination.Get("Proxy-Connection") != "" {
		t.Fatalf("hop headers leaked: %v", destination)
	}
	if destination.Get("Content-Type") != "video/mp4" {
		t.Fatal("end-to-end header was removed")
	}
}

func testLogger(writer io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attribute slog.Attr) slog.Attr {
			if attribute.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attribute
		},
	}))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func transportTo(upstream *httptest.Server) http.RoundTripper {
	base, _ := url.Parse(upstream.URL)
	transport := upstream.Client().Transport
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		forwarded := request.Clone(request.Context())
		forwarded.URL.Scheme = base.Scheme
		forwarded.URL.Host = base.Host
		forwarded.Host = request.URL.Host
		return transport.RoundTrip(forwarded)
	})
}
