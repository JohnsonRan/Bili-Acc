package proxy

import (
	"bytes"
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
	grpcPlayurlHosts  = []string{"grpc.biliapi.net", "app.bilibili.com", "app.biliapi.net"}
	overseaMediaHosts = map[string]bool{
		"upos-hz-mirrorakam.akamaized.net":  true,
		"upos-sz-mirrorawsov.bilivideo.com": true,
		"upos-sz-mirroraliov.bilivideo.com": true,
		"upos-sz-mirrorcosov.bilivideo.com": true,
		"upos-sz-mirrorhwov.bilivideo.com":  true,
	}
)

const grpcPlayurlPath = "/bilibili.app.playerunite.v1.Player/PlayViewUnite"

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
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.Port() != "" || origin.Path != "" || origin.RawQuery != "" || !validDNSHostname(origin.Hostname()) {
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
	return s.fetchAllowedBody(ctx, method, target, headers, nil, allowed)
}

func (s *server) fetchAllowedBody(ctx context.Context, method string, target *url.URL, headers http.Header, body []byte, allowed func(*url.URL) bool) (*http.Response, *url.URL, error) {
	for redirects := 0; redirects <= 5; redirects++ {
		if !allowed(target) {
			return nil, nil, errors.New("redirect host not allowed")
		}
		var requestBody io.Reader
		if body != nil {
			requestBody = bytes.NewReader(body)
		}
		request, err := http.NewRequestWithContext(ctx, method, target.String(), requestBody)
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

func normalizeOverseaMediaTarget(target *url.URL) (*url.URL, bool) {
	host, ok := normalizeDNSHostname(target.Hostname())
	if !ok || !overseaMediaHosts[host] {
		return target, false
	}
	normalized := *target
	normalized.Host = "upos-sz-mirrorali.bilivideo.com"
	return &normalized, true
}

func allowedHost(hostname string, suffixes []string) bool {
	name, ok := normalizeDNSHostname(hostname)
	if !ok {
		return false
	}
	for _, suffix := range suffixes {
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}
	return false
}

func allowedPlayurlHost(target *url.URL) bool {
	host, ok := normalizeDNSHostname(target.Hostname())
	return ok && (host == "api.bilibili.com" || host == "api.live.bilibili.com")
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

func allowedGRPCPlayurl(target *url.URL) bool {
	if target.Scheme != "https" || target.Path != grpcPlayurlPath {
		return false
	}
	host, ok := normalizeDNSHostname(target.Hostname())
	if !ok {
		return false
	}
	for _, allowed := range grpcPlayurlHosts {
		if host == allowed {
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
		host = strings.TrimPrefix(strings.TrimSpace(host), ".")
		if value, ok := normalizeDNSHostname(host); ok {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

func validDNSHostname(hostname string) bool {
	_, ok := normalizeDNSHostname(hostname)
	return ok
}

func normalizeDNSHostname(hostname string) (string, bool) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	if name == "" || len(name) > 253 {
		return "", false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || !isDNSAlphaNumeric(label[0]) || !isDNSAlphaNumeric(label[len(label)-1]) {
			return "", false
		}
		for index := 1; index < len(label)-1; index++ {
			character := label[index]
			if !isDNSAlphaNumeric(character) && character != '-' {
				return "", false
			}
		}
	}
	return name, true
}

func isDNSAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}

func diagnosticHost(hostname string) string {
	if value, ok := normalizeDNSHostname(hostname); ok {
		return value
	}
	if strings.TrimSpace(hostname) == "" {
		return ""
	}
	return "invalid-host"
}

func splitHosts(value string) []string {
	if strings.TrimSpace(value) == "" {
		return normalizeHosts(defaultMediaHosts)
	}
	return normalizeHosts(strings.Split(value, ","))
}
