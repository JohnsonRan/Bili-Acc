package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

const testToken = "test-token"

func encodeTestVarint(value uint64) []byte {
	output := []byte{}
	for {
		current := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			current |= 0x80
		}
		output = append(output, current)
		if value == 0 {
			return output
		}
	}
}

func protobufBytesField(number uint64, value []byte) []byte {
	output := append(encodeTestVarint(number<<3|2), encodeTestVarint(uint64(len(value)))...)
	return append(output, value...)
}

func grpcRequestFrame(payload []byte, compressed bool) []byte {
	framedPayload := payload
	flag := byte(0)
	if compressed {
		var buffer bytes.Buffer
		writer := gzip.NewWriter(&buffer)
		_, _ = writer.Write(payload)
		_ = writer.Close()
		framedPayload = buffer.Bytes()
		flag = 1
	}
	output := make([]byte, 5+len(framedPayload))
	output[0] = flag
	binary.BigEndian.PutUint32(output[1:5], uint32(len(framedPayload)))
	copy(output[5:], framedPayload)
	return output
}

func proxyPath(prefix, token, target string) string {
	u, _ := url.Parse(target)
	origin := u.Scheme + "://" + u.Host
	return prefix + token + "/" + base64.RawURLEncoding.EncodeToString([]byte(origin)) + u.RequestURI()
}

func mustParseTestURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestNormalizePlayViewUniteRequestRemovesAdExtraAndPreservesUnknownFields(t *testing.T) {
	vod := append([]byte{0x08, 0x7b}, protobufBytesField(99, []byte("unknown-vod"))...)
	payload := append(protobufBytesField(1, vod), protobufBytesField(playViewUniteAdExtraField, []byte("client-ad-context"))...)
	payload = append(payload, protobufBytesField(99, []byte("unknown-request"))...)
	framed := grpcRequestFrame(payload, false)

	normalized, changed := normalizePlayViewUniteRequest(framed, "")
	if !changed || normalized[0] != 0 {
		t.Fatalf("changed=%v flag=%d", changed, normalized[0])
	}
	if bytes.Contains(normalized, []byte("client-ad-context")) {
		t.Fatal("ad_extra was retained")
	}
	for _, expected := range [][]byte{vod, []byte("unknown-vod"), []byte("unknown-request")} {
		if !bytes.Contains(normalized, expected) {
			t.Fatalf("missing preserved bytes %q", expected)
		}
	}
	if got := int(binary.BigEndian.Uint32(normalized[1:5])); got != len(normalized)-5 {
		t.Fatalf("frame length=%d body=%d", got, len(normalized)-5)
	}
}

func TestNormalizePlayViewUniteRequestPreservesCapturedRequestFields(t *testing.T) {
	expected, err := os.ReadFile("testdata/playviewunite-no-ad-extra.bin")
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte(nil), expected[5:]...)
	payload = append(payload, protobufBytesField(playViewUniteAdExtraField, bytes.Repeat([]byte("A"), 2272))...)
	normalized, changed := normalizePlayViewUniteRequest(grpcRequestFrame(payload, true), "gzip")
	if !changed || !bytes.Equal(normalized, expected) {
		t.Fatalf("changed=%v normalized=%x want=%x", changed, normalized, expected)
	}
}

func TestNormalizePlayViewUniteRequestDecompressesGzipFrame(t *testing.T) {
	payload := append(protobufBytesField(5, []byte("BV1")), protobufBytesField(playViewUniteAdExtraField, []byte("client-ad-context"))...)
	framed := grpcRequestFrame(payload, true)
	normalized, changed := normalizePlayViewUniteRequest(framed, "gzip")
	if !changed || normalized[0] != 0 || bytes.Contains(normalized, []byte("client-ad-context")) || !bytes.Contains(normalized, []byte("BV1")) {
		t.Fatalf("changed=%v flag=%d body=%x", changed, normalized[0], normalized)
	}
}

func TestNormalizePlayViewUniteRequestFallsBackOnUnsupportedFrames(t *testing.T) {
	compressed := grpcRequestFrame(protobufBytesField(playViewUniteAdExtraField, []byte("ad")), true)
	reservedCompressed := append([]byte(nil), compressed...)
	reservedCompressed[0] = 3
	reservedIdentity := grpcRequestFrame(protobufBytesField(playViewUniteAdExtraField, []byte("ad")), false)
	reservedIdentity[0] = 2
	cases := [][]byte{
		{0, 0, 0, 0, 2, 8},
		compressed,
		reservedCompressed,
		reservedIdentity,
		append(grpcRequestFrame(protobufBytesField(playViewUniteAdExtraField, []byte("ad")), false), 0),
	}
	for index, body := range cases {
		normalized, changed := normalizePlayViewUniteRequest(body, "br")
		if changed || !bytes.Equal(normalized, body) {
			t.Fatalf("case=%d changed=%v", index, changed)
		}
	}
}

func TestMediaGroupRetriesConnectionErrorsAndUpstream5xx(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	attempts := []string{}
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts = append(attempts, request.URL.Host)
		switch request.URL.Host {
		case "first.bilivideo.com":
			return nil, errors.New("dial failed")
		case "second.bilivideo.com":
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("bad")), Request: request}, nil
		default:
			return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
		}
	})
	registration := `{"groups":[{"urls":["https://first.bilivideo.com/video.m4s","https://second.bilivideo.com/video.m4s","https://third.bilivideo.com/video.m4s"]}]}`
	registerRequest := httptest.NewRequest(http.MethodPost, "/media-groups/"+testToken, strings.NewReader(registration))
	registerResponse := httptest.NewRecorder()
	app.ServeHTTP(registerResponse, registerRequest)
	if registerResponse.Code != http.StatusOK {
		t.Fatalf("registration status = %d body=%q", registerResponse.Code, registerResponse.Body.String())
	}
	var registered mediaGroupRegistrationResponse
	if err := json.Unmarshal(registerResponse.Body.Bytes(), &registered); err != nil || len(registered.IDs) != 1 {
		t.Fatalf("registration = %#v err=%v", registered, err)
	}
	request := httptest.NewRequest(http.MethodGet, "/proxy-group/"+testToken+"/"+registered.IDs[0]+"/0", nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "ok" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if got := strings.Join(attempts, ","); got != "first.bilivideo.com,second.bilivideo.com,third.bilivideo.com" {
		t.Fatalf("attempts = %q", got)
	}
	window := app.metrics.snapshot(app.now()).Windows["15m"]
	if window.CandidateAttempts != 3 || window.Fallbacks != 1 || window.FallbackRecoveries != 1 || window.CandidateExhausted != 0 {
		t.Fatalf("candidate metrics=%+v", window)
	}
}

func TestMediaGroupDeprioritizesLikelyIPBoundCOSCandidate(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	urls := []*url.URL{
		mustParseTestURL(t, "https://upos-sz-mirrorcosov.bilivideo.com/video.m4s?gen=playurlv3&oi=2582799872"),
		mustParseTestURL(t, "https://upos-sz-mirrorali.bilivideo.com/video.m4s?gen=playurlv3&oi=2582799872"),
		mustParseTestURL(t, "https://upos-hz-mirrorakam.akamaized.net/video.m4s?gen=playurlv3&oi=2582799872"),
		mustParseTestURL(t, "https://upos-sz-mirrorcosov.bilivideo.com/legacy.m4s?gen=playurl&oi=2582799872"),
	}
	app.mediaGroups["0123456789abcdef0123456789abcdef"] = mediaGroup{URLs: urls, CreatedAt: app.now(), ExpiresAt: app.now().Add(time.Minute)}

	candidates, ok := app.mediaGroupCandidates("0123456789abcdef0123456789abcdef", 0)
	if !ok {
		t.Fatal("media group not found")
	}
	got := make([]string, len(candidates))
	for index, candidate := range candidates {
		got[index] = candidate.Hostname() + candidate.Path
	}
	want := []string{
		"upos-sz-mirrorali.bilivideo.com/video.m4s",
		"upos-hz-mirrorakam.akamaized.net/video.m4s",
		"upos-sz-mirrorcosov.bilivideo.com/legacy.m4s",
		"upos-sz-mirrorcosov.bilivideo.com/video.m4s",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("candidate order=%v want=%v", got, want)
	}
}

func TestMediaGroupKeepsRiskyCandidatesWhenNoAlternativeExists(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	first := mustParseTestURL(t, "https://first-mirrorcosov.bilivideo.com/first.m4s?gen=playurlv3&oi=1")
	second := mustParseTestURL(t, "https://second-mirrorcosov.bilivideo.com/second.m4s?gen=playurlv3&oi=2")
	app.mediaGroups["0123456789abcdef0123456789abcdef"] = mediaGroup{URLs: []*url.URL{first, second}, CreatedAt: app.now(), ExpiresAt: app.now().Add(time.Minute)}

	candidates, ok := app.mediaGroupCandidates("0123456789abcdef0123456789abcdef", 1)
	if !ok || len(candidates) != 2 || candidates[0].Hostname() != second.Hostname() || candidates[1].Hostname() != first.Hostname() {
		t.Fatalf("candidates=%v ok=%v", candidates, ok)
	}
}

func TestLikelyIPBoundCOSCandidateRequiresAllSignals(t *testing.T) {
	for _, test := range []struct {
		url  string
		want bool
	}{
		{"https://upos-sz-mirrorcosov.bilivideo.com/video.m4s?gen=playurlv3&oi=2582799872", true},
		{"https://upos-sz-mirrorcosov.bilivideo.com/video.m4s?gen=playurlv3", false},
		{"https://upos-sz-mirrorcosov.bilivideo.com/video.m4s?gen=playurl&oi=2582799872", false},
		{"https://upos-sz-mirrorali.bilivideo.com/video.m4s?gen=playurlv3&oi=2582799872", false},
	} {
		if got := likelyIPBoundCOSCandidate(mustParseTestURL(t, test.url)); got != test.want {
			t.Fatalf("url=%s got=%v want=%v", test.url, got, test.want)
		}
	}
}

func TestMediaGroupHealthRankingPrefersSuccessfulFastCandidate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return now }
	first := mustParseTestURL(t, "https://first.bilivideo.com/video.m4s")
	second := mustParseTestURL(t, "https://second.bilivideo.com/video.m4s")
	third := mustParseTestURL(t, "https://third.bilivideo.com/video.m4s")
	app.recordCandidateSuccess(first, 400*time.Millisecond)
	app.recordCandidateSuccess(second, 80*time.Millisecond)
	app.recordCandidateFailure(third, http.StatusForbidden)

	ordered := app.rankMediaCandidates([]*url.URL{first, third, second}, now)
	got := []string{ordered[0].Hostname(), ordered[1].Hostname(), ordered[2].Hostname()}
	want := []string{"second.bilivideo.com", "first.bilivideo.com", "third.bilivideo.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("candidate order=%v want=%v", got, want)
	}
}

func TestMediaGroupHealthRankingNeverPromotesRiskyCOS(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return now }
	safe := mustParseTestURL(t, "https://safe.bilivideo.com/video.m4s?gen=playurlv3&oi=1")
	risky := mustParseTestURL(t, "https://fast-mirrorcosov.bilivideo.com/video.m4s?gen=playurlv3&oi=1")
	app.recordCandidateSuccess(risky, time.Millisecond)
	app.recordCandidateSuccess(safe, time.Second)
	ordered := app.rankMediaCandidates([]*url.URL{risky, safe}, now)
	if ordered[0].Hostname() != safe.Hostname() {
		t.Fatalf("risky candidate promoted: %v", ordered)
	}
}

func TestIdleTimeoutReadCloserStopsStalledBody(t *testing.T) {
	body := &blockingReadCloser{closed: make(chan struct{})}
	reader := newIdleTimeoutReadCloser(body, 10*time.Millisecond)
	started := time.Now()
	_, err := reader.Read(make([]byte, 1))
	if !errors.Is(err, errUpstreamIdleTimeout) {
		t.Fatalf("read error=%v", err)
	}
	if elapsed := time.Since(started); elapsed < 10*time.Millisecond || elapsed > time.Second {
		t.Fatalf("idle timeout elapsed=%s", elapsed)
	}
}

func TestMediaGroupRetriesUpstream403(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	attempts := []string{}
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts = append(attempts, request.URL.Host)
		if request.URL.Host == "first.bilivideo.com" {
			return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("forbidden")), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	registration := `{"groups":[{"urls":["https://first.bilivideo.com/video.m4s","https://second.bilivideo.com/video.m4s"]}]}`
	registerResponse := httptest.NewRecorder()
	app.ServeHTTP(registerResponse, httptest.NewRequest(http.MethodPost, "/media-groups/"+testToken, strings.NewReader(registration)))
	var registered mediaGroupRegistrationResponse
	_ = json.Unmarshal(registerResponse.Body.Bytes(), &registered)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/proxy-group/"+testToken+"/"+registered.IDs[0]+"/0", nil))
	if response.Code != http.StatusPartialContent || response.Body.String() != "ok" {
		t.Fatalf("response=%d body=%q", response.Code, response.Body.String())
	}
	if got := strings.Join(attempts, ","); got != "first.bilivideo.com,second.bilivideo.com" {
		t.Fatalf("attempts=%q", got)
	}
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
	requestPayload := append(protobufBytesField(5, []byte("BV1")), protobufBytesField(playViewUniteAdExtraField, []byte("client-ad-context"))...)
	requestBody := grpcRequestFrame(requestPayload, true)
	expectedUpstreamBody := grpcRequestFrame(protobufBytesField(5, []byte("BV1")), false)
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
		if err != nil || !bytes.Equal(body, expectedUpstreamBody) {
			t.Fatalf("body = %x want %x err=%v", body, expectedUpstreamBody, err)
		}
		if got := r.Header.Get("Grpc-Encoding"); got != "" {
			t.Fatalf("Grpc-Encoding = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc+proto" {
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
		for _, name := range []string{grpcOriginalContentTypeHdr, "X-Bili-Cookie", "X-Internal", "Proxy-Connection", "Forwarded", "X-Forwarded-For", "X-Real-Ip", "Cf-Connecting-Ip"} {
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
	request.Header.Set("Content-Type", grpcTunnelContentType)
	request.Header.Set(grpcOriginalContentTypeHdr, "application/grpc+proto")
	request.Header.Set("Grpc-Accept-Encoding", "gzip,identity")
	request.Header.Set("Grpc-Encoding", "gzip")
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
	if result.Header.Get("Content-Type") != "application/grpc" || result.Header.Get("Content-Length") != "" || result.Header.Get(grpcTunnelStatusHdr) != "0" {
		t.Fatalf("headers=%v", result.Header)
	}
	if result.Trailer.Get("Grpc-Status") != "0" || result.Trailer.Get("X-Rpc-Metadata") != "complete" {
		t.Fatalf("trailers=%v", result.Trailer)
	}
}

func TestGRPCPlayurlProxyMapsMalformedFinalStatusToUnknown(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0, 0, 0, 0, 0})
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "malformed")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	target := "https://grpc.biliapi.net" + grpcPlayurlPath
	request := httptest.NewRequest(http.MethodPost, proxyPath("/playurl-grpc/", testToken, target), bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	request.Header.Set("Content-Type", grpcTunnelContentType)
	request.Header.Set(grpcOriginalContentTypeHdr, "application/grpc")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	result := response.Result()
	defer result.Body.Close()
	_, _ = io.ReadAll(result.Body)
	if result.Header.Get(grpcTunnelStatusHdr) != "2" {
		t.Fatalf("tunneled status = %q headers=%v trailers=%v", result.Header.Get(grpcTunnelStatusHdr), result.Header, result.Trailer)
	}
}

func TestGRPCPlayurlProxyMapsMissingStatusToUnknown(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/grpc"}},
			Body:       io.NopCloser(bytes.NewReader([]byte{0, 0, 0, 0, 0})),
			Request:    request,
		}, nil
	})
	target := "https://grpc.biliapi.net" + grpcPlayurlPath
	request := httptest.NewRequest(http.MethodPost, proxyPath("/playurl-grpc/", testToken, target), bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	request.Header.Set("Content-Type", grpcTunnelContentType)
	request.Header.Set(grpcOriginalContentTypeHdr, "application/grpc")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	result := response.Result()
	defer result.Body.Close()
	_, _ = io.ReadAll(result.Body)
	if result.Header.Get(grpcTunnelStatusHdr) != "2" {
		t.Fatalf("tunneled status = %q", result.Header.Get(grpcTunnelStatusHdr))
	}
}

func TestNormalizeGRPCStatusForWire(t *testing.T) {
	if value, ok := headerValue(http.Header{"Grpc-Status": {""}}, "Grpc-Status"); !ok || value != "" {
		t.Fatalf("empty announced trailer value=%q present=%t", value, ok)
	}
	for input, expected := range map[string]string{"": "2", "0": "0", "16": "16", "17": "2", "-1": "2", "bad": "2"} {
		if got := normalizeGRPCStatusForWire(input); got != expected {
			t.Fatalf("input=%q got=%q expected=%q", input, got, expected)
		}
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

func TestGRPCPlayurlProxyEnforcesResponseSizeLimit(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/grpc"}},
			Body:       io.NopCloser(bytes.NewReader(make([]byte, maxGRPCResponseSize+1))),
			Request:    request,
		}, nil
	})
	target := "https://grpc.biliapi.net" + grpcPlayurlPath
	request := httptest.NewRequest(http.MethodPost, proxyPath("/playurl-grpc/", testToken, target), bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	request.Header.Set("Content-Type", grpcTunnelContentType)
	request.Header.Set(grpcOriginalContentTypeHdr, "application/grpc")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%q", response.Code, response.Body.String())
	}
}

func TestPlayurlRequestsHighestQualityWithoutChangingResponseMetadata(t *testing.T) {
	upstreamBody := `{"code":0,"data":{"quality":80,"accept_quality":[127,120,80]}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != qualityUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
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

func TestSummarizeLivePlayurlQuality(t *testing.T) {
	var root any
	body := `{"code":0,"data":{"playurl_info":{"playurl":{"current_qn":10000,"accept_qn":[30000,10000,400],"quality_description":[{"qn":30000,"desc":"杜比"},{"qn":10000,"desc":"原画"},{"qn":400,"desc":"蓝光"}],"stream":[{"protocol_name":"http_hls","format":[{"format_name":"fmp4","codec":[{"codec_name":"av1","current_qn":10000,"base_url":"/live/index.m3u8","url_info":[{"host":"https://live.bilivideo.com","extra":"?qn=10000"}]}]}]}]}}}}`
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		t.Fatal(err)
	}
	summary := summarizePlayurl(root)
	if summary.Quality != 10000 || summary.QualityLabel != "原画" || summary.AcceptQualities != "30000,10000,400" || summary.VideoCodecs != "av1" {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestLivePlayurlDisablesCaching(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") != "no-cache" || r.Header.Get("Pragma") != "no-cache" {
			t.Fatalf("cache headers = %v", r.Header)
		}
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("Age", "30")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"quality":10000}}`)
	}))
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, "https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo?room_id=1"), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Age") != "" {
		t.Fatalf("status=%d headers=%v", response.Code, response.Header())
	}
}

func TestPlayurlResponseRegistersFallbackGroupsAndLogsActualQuality(t *testing.T) {
	upstreamBody := `{"code":0,"data":{"quality":120,"accept_quality":[127,120,80],"dash":{"video":[{"id":120,"codecs":"hev1.1.6.L150.90","baseUrl":"https://upos-sz-mirrorali.bilivideo.com/video.m4s?sig=1","backupUrl":["https://upos-sz-302ppio.bilivideo.com/video.m4s?sig=2"]},{"id":80,"codecs":"avc1.640032","baseUrl":"https://cdn.bilivideo.com/1080.m4s?sig=3"}]}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.logger = testLogger(&logs)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/playurl/", testToken, "https://api.bilibili.com/x/player/playurl?cid=1"), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	var parsed struct {
		Data struct {
			Dash struct {
				Video []struct {
					BaseURL   string   `json:"baseUrl"`
					BackupURL []string `json:"backupUrl"`
				} `json:"video"`
			} `json:"dash"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	first := parsed.Data.Dash.Video[0]
	if !strings.Contains(first.BaseURL, "/proxy-group/") || !strings.HasSuffix(first.BaseURL, "/0") || len(first.BackupURL) != 1 || !strings.HasSuffix(first.BackupURL[0], "/1") {
		t.Fatalf("rewritten group = %+v", first)
	}
	if got := parsed.Data.Dash.Video[1].BaseURL; got != "https://cdn.bilivideo.com/1080.m4s?sig=3" {
		t.Fatalf("single candidate rewritten = %q", got)
	}
	logged := logs.String()
	for _, expected := range []string{"actual_quality=120", "accept_quality=127,120,80", "video_qualities=120,80", "video_codecs=avc1.640032,hev1.1.6.L150.90", "media_groups=1"} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("log missing %q: %q", expected, logged)
		}
	}
	for _, secret := range []string{"sig=1", "sig=2", "video.m4s"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %q", secret, logged)
		}
	}
}

func TestCandidateRankingDeprioritizesP2PCDN(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	candidates := []*url.URL{
		mustParseTestURL(t, "https://upos-sz-302ppio.bilivideo.com/video.m4s"),
		mustParseTestURL(t, "https://cdn.bilivideo.com/video.m4s"),
		mustParseTestURL(t, "https://node.mcdn.bilivideo.cn/video.m4s"),
		mustParseTestURL(t, "https://upos-sz-mirrorcosov.bilivideo.com/video.m4s?gen=playurlv3&oi=1"),
	}
	ordered := app.rankMediaCandidates(candidates, time.Now())
	if got := ordered[0].Hostname(); got != "cdn.bilivideo.com" {
		t.Fatalf("first candidate = %q", got)
	}
	if !likelyP2PCandidate(ordered[1]) || !likelyP2PCandidate(ordered[2]) || !likelyIPBoundCOSCandidate(ordered[3]) {
		t.Fatalf("candidate order = %v", ordered)
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
		if r.Header.Get("Cache-Control") != "no-cache" || r.Header.Get("Pragma") != "no-cache" {
			t.Fatalf("cache headers = %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "segment.ts")
	}))
	defer upstream.Close()

	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://live.bilivideo.com/live-stream"), nil)
	request.Header.Set("Accept", "application/vnd.apple.mpegurl")
	request.Header.Set("Range", "bytes=0-100")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestLivePlaylistResponseIsTrimmedAndNotCached(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("Age", "30")
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:4,\n10.ts\n#EXTINF:4,\n11.ts\n#EXTINF:4,\n12.ts\n#EXTINF:4,\n13.ts\n")
	}))
	defer upstream.Close()

	now := time.Unix(1_700_000_000, 0)
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	app.now = func() time.Time { return now }
	app.metrics = newMetricsStore(now)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://live.bilivideo.com/index.m3u8"), nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Age") != "" {
		t.Fatalf("status=%d headers=%v", response.Code, response.Header())
	}
	if strings.Contains(response.Body.String(), "10.ts") || !strings.Contains(response.Body.String(), "#EXT-X-MEDIA-SEQUENCE:11") {
		t.Fatalf("playlist = %q", response.Body.String())
	}
	window := app.metrics.snapshot(now).Windows["1m"]
	if window.LivePlaylists != 1 || window.PlaylistTrims != 1 || window.SegmentsSkipped != 1 || window.PlaylistTrimErrors != 0 {
		t.Fatalf("window = %+v", window)
	}
}

func TestPlaylistRequiresConfiguredPublicURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, "segment.ts")
	}))
	defer upstream.Close()

	app := newServer(testToken, "", defaultMediaHosts)
	app.client.Transport = transportTo(upstream)
	request := httptest.NewRequest(http.MethodGet, proxyPath("/proxy/", testToken, "http://live.bilivideo.com/index.m3u8"), nil)
	request.Host = "attacker.example"
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	if strings.Contains(response.Body.String(), testToken) || strings.Contains(response.Body.String(), "attacker.example") {
		t.Fatalf("response leaked proxy details: %q", response.Body.String())
	}
}

func TestCredentialedCORSPreflight(t *testing.T) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	request := httptest.NewRequest(http.MethodOptions, "/playurl/anything", nil)
	request.Header.Set("Origin", "https://www.bilibili.com")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	request.Header.Set("Access-Control-Request-Headers", "X-Bili-Cookie, X-Bili-Referer")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://www.bilibili.com" {
		t.Fatalf("origin = %q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("credentials = %q", got)
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

func TestLivePlaylistKeepsLatestCompleteSegments(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:100\n#EXT-X-KEY:METHOD=AES-128,URI=\"old.key\"\n#EXT-X-PROGRAM-DATE-TIME:2026-08-13T23:59:40Z\n#EXTINF:4,\n100.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:4,\n101.ts\n#EXT-X-KEY:METHOD=AES-128,URI=\"new.key\"\n#EXTINF:4,\n102.ts\n#EXTINF:4,\n103.ts\n#EXTINF:4,\n104.ts\n#EXT-X-PROGRAM-DATE-TIME:2026-08-14T00:00:00Z\n#EXTINF:4,\n105.ts\n"
	trimmed, result := trimLivePlaylist(body, 3)
	if !result.Live || !result.Trimmed || result.Skipped != 3 || result.Malformed {
		t.Fatalf("result = %+v", result)
	}
	for _, expected := range []string{"#EXT-X-MEDIA-SEQUENCE:103", "#EXT-X-DISCONTINUITY-SEQUENCE:1", "URI=\"new.key\"", "103.ts", "104.ts", "105.ts", "#EXT-X-PROGRAM-DATE-TIME"} {
		if !strings.Contains(trimmed, expected) {
			t.Fatalf("trimmed playlist missing %q: %q", expected, trimmed)
		}
	}
	for _, removed := range []string{"100.ts", "101.ts", "102.ts", "old.key", "23:59:40Z"} {
		if strings.Contains(trimmed, removed) {
			t.Fatalf("trimmed playlist retained %q: %q", removed, trimmed)
		}
	}
}

func TestFMP4LivePlaylistKeepsMapAndLatestSegments(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-START:TIME-OFFSET=0\n#EXT-X-MEDIA-SEQUENCE:119413416\n#EXT-X-TARGETDURATION:1\n#EXT-X-MAP:URI=\"init.m4s\"\n#EXT-BILI-AUX:one\n#EXTINF:1.00,\n119413416.m4s\n#EXT-BILI-AUX:two\n#EXTINF:1.00,\n119413417.m4s\n#EXT-BILI-AUX:three\n#EXTINF:1.00,\n119413418.m4s\n#EXT-BILI-AUX:four\n#EXTINF:1.00,\n119413419.m4s\n#EXT-BILI-AUX:five\n#EXTINF:1.00,\n119413420.m4s\n"
	trimmed, result := trimLivePlaylist(body, 3)
	if !result.Live || !result.Trimmed || result.Skipped != 2 || result.Malformed {
		t.Fatalf("result = %+v", result)
	}
	for _, expected := range []string{"#EXT-X-MAP:URI=\"init.m4s\"", "#EXT-X-MEDIA-SEQUENCE:119413418", "#EXT-BILI-AUX:three", "119413418.m4s", "119413419.m4s", "119413420.m4s"} {
		if !strings.Contains(trimmed, expected) {
			t.Fatalf("trimmed playlist missing %q: %q", expected, trimmed)
		}
	}
	for _, removed := range []string{"#EXT-X-START:", "#EXT-BILI-AUX:one", "#EXT-BILI-AUX:two", "119413416.m4s", "119413417.m4s"} {
		if strings.Contains(trimmed, removed) {
			t.Fatalf("trimmed playlist retained %q: %q", removed, trimmed)
		}
	}
}

func TestUnsafePlaylistsAreNotTrimmed(t *testing.T) {
	for _, marker := range []string{"#EXT-X-ENDLIST", "#EXT-X-PLAYLIST-TYPE:EVENT", "#EXT-X-BYTERANGE:100", "#EXT-X-MAP:URI=\"init-a.mp4\"\n#EXT-X-MAP:URI=\"init-b.mp4\"", "#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\""} {
		body := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n" + marker + "\n#EXTINF:4,\n1.ts\n#EXTINF:4,\n2.ts\n#EXTINF:4,\n3.ts\n#EXTINF:4,\n4.ts\n"
		trimmed, result := trimLivePlaylist(body, 3)
		if trimmed != body || result.Live || result.Trimmed {
			t.Fatalf("marker=%q trimmed=%q result=%+v", marker, trimmed, result)
		}
	}
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

func TestNormalizeGRPCMetadataRemovesRemoteFingerprintAndClientNetworkHints(t *testing.T) {
	device := []byte{0x08, 0x01, 0x62, 0x06, 'r', 'e', 'm', 'o', 't', 'e', 0x6a, 0x07, 'v', 'e', 'r', 's', 'i', 'o', 'n'}
	headers := http.Header{
		"X-Bili-Device-Bin":  {base64.StdEncoding.EncodeToString(device)},
		"X-Bili-Network-Bin": {base64.StdEncoding.EncodeToString([]byte{0x08, 0x02, 0x18, 0x01, 0x1a, 0x03, '4', '4', '0'})},
		"X-Bili-Aurora-Zone": {"jp001"},
		"X-Bili-Exps-Bin":    {"experiment"},
		"Authorization":      {"identify_v1 secret"},
	}
	normalizeGRPCMetadata(headers)
	decoded, err := base64.StdEncoding.DecodeString(headers.Get("X-Bili-Device-Bin"))
	if err != nil || !bytes.Equal(decoded, []byte{0x08, 0x01, 0x6a, 0x07, 'v', 'e', 'r', 's', 'i', 'o', 'n'}) {
		t.Fatalf("device metadata = %x err=%v", decoded, err)
	}
	if headers.Get("X-Bili-Network-Bin") != "CAE=" || headers.Get("X-Bili-Aurora-Zone") != "" || headers.Get("X-Bili-Exps-Bin") != "" {
		t.Fatalf("normalized headers = %v", headers)
	}
	if headers.Get("Authorization") != "identify_v1 secret" {
		t.Fatal("authorization was removed")
	}
}

func TestNormalizeGRPCMetadataPreservesMalformedDeviceValue(t *testing.T) {
	headers := http.Header{"X-Bili-Device-Bin": {"not-base64"}}
	normalizeGRPCMetadata(headers)
	if headers.Get("X-Bili-Device-Bin") != "not-base64" {
		t.Fatalf("device metadata = %q", headers.Get("X-Bili-Device-Bin"))
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
