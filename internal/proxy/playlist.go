package proxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var playlistURI = regexp.MustCompile(`URI="([^"]+)"`)

func (s *server) rewritePlaylist(body string, baseURL *url.URL, publicBase string) (string, error) {
	rewrite := func(value string) (string, error) {
		target, err := baseURL.Parse(value)
		if err != nil || !allowedHost(target.Hostname(), s.mediaHosts) {
			return "", errors.New("playlist host not allowed")
		}
		origin := target.Scheme + "://" + target.Host
		encoded := base64.RawURLEncoding.EncodeToString([]byte(origin))
		return fmt.Sprintf("%s/proxy/%s/%s%s", publicBase, url.PathEscape(s.token), encoded, target.RequestURI()), nil
	}

	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			value, err := rewrite(trimmed)
			if err != nil {
				return "", err
			}
			lines[i] = value
			continue
		}
		var rewriteErr error
		lines[i] = playlistURI.ReplaceAllStringFunc(line, func(match string) string {
			parts := playlistURI.FindStringSubmatch(match)
			value, err := rewrite(parts[1])
			if err != nil {
				rewriteErr = err
				return match
			}
			return `URI="` + value + `"`
		})
		if rewriteErr != nil {
			return "", rewriteErr
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (s *server) publicBase(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme != "http" && scheme != "https" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + r.Host
}

func isPlaylist(contentType, path string) bool {
	return strings.Contains(strings.ToLower(contentType), "mpegurl") || strings.HasSuffix(strings.ToLower(path), ".m3u8")
}
