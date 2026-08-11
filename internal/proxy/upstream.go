package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	defaultMediaHosts = []string{"bilivideo.com", "bilivideo.cn", "biliapi.net", "akamaized.net"}
	playurlPaths      = []*regexp.Regexp{
		regexp.MustCompile(`^/x/player/(?:wbi/)?playurl$`),
		regexp.MustCompile(`^/pgc/player/web(?:/v2)?/playurl$`),
		regexp.MustCompile(`^/xlive/web-room/v2/index/getRoomPlayInfo$`),
		regexp.MustCompile(`^/room/v1/Room/playUrl$`),
	}
)

const (
	redirectDrainMax     = 2 << 10
	redirectDrainTimeout = 250 * time.Millisecond
)

func (s *server) targetFromRequest(r *http.Request, prefix string) (*url.URL, error) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return nil, errors.New("invalid proxy URL")
	}
	token, err := url.PathUnescape(parts[0])
	if err != nil || !secureEqual(token, s.token) {
		return nil, errors.New("unauthorized")
	}
	encodedOrigin, err := url.PathUnescape(parts[1])
	if err != nil {
		return nil, errors.New("invalid origin")
	}
	originBytes, err := base64.RawURLEncoding.DecodeString(encodedOrigin)
	if err != nil {
		return nil, errors.New("invalid origin")
	}
	origin, err := url.Parse(string(originBytes))
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.Port() != "" || origin.Path != "" || origin.RawQuery != "" {
		return nil, errors.New("invalid origin")
	}
	path := "/"
	if len(parts) == 3 {
		path += parts[2]
	}
	target, err := url.Parse(origin.String() + path)
	if err != nil {
		return nil, errors.New("invalid target")
	}
	target.RawQuery = r.URL.RawQuery
	return target, nil
}

func (s *server) fetchAllowed(ctx context.Context, method string, target *url.URL, headers http.Header, allowed func(*url.URL) bool) (*http.Response, *url.URL, error) {
	for redirects := 0; redirects <= 5; redirects++ {
		if !allowed(target) {
			return nil, nil, errors.New("redirect host not allowed")
		}
		request, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		request.Header = headers.Clone()
		response, err := s.client.Do(request)
		if err != nil {
			return nil, nil, err
		}
		if !isRedirect(response.StatusCode) {
			return response, target, nil
		}
		location := response.Header.Get("Location")
		if location == "" {
			return response, target, nil
		}
		closeRedirectBody(response)
		target, err = target.Parse(location)
		if err != nil {
			return nil, nil, errors.New("invalid redirect")
		}
	}
	return nil, nil, errors.New("too many redirects")
}

func closeRedirectBody(response *http.Response) {
	if response.ContentLength == 0 || response.ContentLength > redirectDrainMax || response.ContentLength < 0 {
		response.Body.Close()
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.CopyN(io.Discard, response.Body, redirectDrainMax)
		close(done)
	}()
	timer := time.NewTimer(redirectDrainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		response.Body.Close()
		<-done
	}
	response.Body.Close()
}

func allowedHost(hostname string, suffixes []string) bool {
	name := strings.ToLower(strings.TrimSuffix(hostname, "."))
	for _, suffix := range suffixes {
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}
	return false
}

func allowedPlayurlHost(target *url.URL) bool {
	host := strings.ToLower(target.Hostname())
	return host == "api.bilibili.com" || host == "api.live.bilibili.com"
}

func allowedPlayurl(target *url.URL) bool {
	if target.Scheme != "https" || !allowedPlayurlHost(target) {
		return false
	}
	for _, pattern := range playurlPaths {
		if pattern.MatchString(target.Path) {
			return true
		}
	}
	return false
}

func secureEqual(value, expected string) bool {
	if expected == "" || len(value) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

func isRedirect(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusFound || status == http.StatusSeeOther || status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect
}

func normalizeHosts(hosts []string) []string {
	normalized := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(host), "."))
		if host != "" {
			normalized = append(normalized, host)
		}
	}
	return normalized
}

func splitHosts(value string) []string {
	if strings.TrimSpace(value) == "" {
		return normalizeHosts(defaultMediaHosts)
	}
	return normalizeHosts(strings.Split(value, ","))
}
