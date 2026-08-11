package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
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
	target := "https://api.bilibili.com/x/player/playurl?bvid=BV1&cid=1&qn=80"
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, target), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != upstreamBody {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
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
	app.logger = log.New(&output, "", 0)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://cdn.bilivideo.com/video.m4s?secret=query-value"), nil)
	request.Header.Set("Cookie", "SESSDATA=cookie-value")
	request.Header.Set("Range", "bytes=0-0")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)

	logged := output.String()
	for _, expected := range []string{"route=media", `target_host="cdn.bilivideo.com"`, "status=206", "upstream_status=206", "bytes=1", "range=true"} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("log missing %q: %q", expected, logged)
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
	source.Set("X-Internal", "secret")
	source.Set("Content-Type", "video/mp4")
	destination := make(http.Header)
	copyResponseHeaders(destination, source)
	if destination.Get("X-Internal") != "" || destination.Get("Connection") != "" {
		t.Fatalf("hop headers leaked: %v", destination)
	}
	if destination.Get("Content-Type") != "video/mp4" {
		t.Fatal("end-to-end header was removed")
	}
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
