package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func BenchmarkRewritePlaylist(b *testing.B) {
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)
	baseURL, err := url.Parse("https://live.bilivideo.com/live/index.m3u8")
	if err != nil {
		b.Fatal(err)
	}

	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for i := range 100 {
		playlist.WriteString("#EXTINF:2.0,\n")
		playlist.WriteString("segment-")
		playlist.WriteString(string(rune('a' + i%26)))
		playlist.WriteString(".m4s\n")
	}
	body := playlist.String()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := app.rewritePlaylist(body, baseURL, "https://proxy.example"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSignWBI(b *testing.B) {
	values := url.Values{
		"avid":     {"170001"},
		"cid":      {"279786"},
		"fnval":    {"4048"},
		"fourk":    {"1"},
		"qn":       {"127"},
		"platform": {"html5"},
	}
	const mixinKey = "ea1db124af3c7062474693fa704f4ff8"

	b.ReportAllocs()
	for b.Loop() {
		_ = signWBI(values, mixinKey, 1700000000)
	}
}

func BenchmarkCopyResponseHeaders(b *testing.B) {
	source := make(http.Header)
	source.Set("Content-Type", "video/mp4")
	source.Set("Content-Length", "1048576")
	source.Set("Accept-Ranges", "bytes")
	source.Set("Cache-Control", "public, max-age=3600")
	source.Set("Connection", "X-Upstream-Internal")
	source.Set("X-Upstream-Internal", "drop-me")
	source.Set("Access-Control-Allow-Origin", "https://upstream.example")

	b.ReportAllocs()
	for b.Loop() {
		destination := make(http.Header)
		copyResponseHeaders(destination, source)
	}
}

func BenchmarkCopyBody(b *testing.B) {
	body := bytes.Repeat([]byte("bili-acc-stream-"), 1<<16)
	writer := &discardResponseWriter{header: make(http.Header)}
	app := newServer(testToken, "https://proxy.example", defaultMediaHosts)

	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		writer.written = 0
		reader := io.LimitReader(bytes.NewReader(body), int64(len(body)))
		app.copyBody(writer, reader, &requestLog{})
		if writer.written != len(body) {
			b.Fatalf("copied %d bytes, want %d", writer.written, len(body))
		}
	}
}

type discardResponseWriter struct {
	header  http.Header
	written int
}

func (w *discardResponseWriter) Header() http.Header {
	return w.header
}

func (w *discardResponseWriter) Write(body []byte) (int, error) {
	w.written += len(body)
	return len(body), nil
}

func (*discardResponseWriter) WriteHeader(int) {}
