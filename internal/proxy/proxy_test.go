package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

	target := "http://api.bilibili.com/x/player/wbi/playurl?bvid=BV1&cid=1"
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, target), nil)
	request.Header.Set("X-Bili-Cookie", "SESSDATA=secret")
	request.Header.Set("X-Bili-Referer", "https://www.bilibili.com/video/BV1")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != `{"code":0}` {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
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

func transportTo(upstream *httptest.Server) http.RoundTripper {
	dialer := &net.Dialer{}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, upstream.Listener.Addr().String())
		},
	}
}
