package proxy

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

type requestLogKey struct{}

func requestRoute(path string) string {
	switch {
	case path == "/":
		return "health"
	case strings.HasPrefix(path, "/proxy/"):
		return "media"
	case strings.HasPrefix(path, "/playurl/"):
		return "playurl"
	default:
		return "not_found"
	}
}

func requestLogFrom(r *http.Request) *requestLog {
	meta, _ := r.Context().Value(requestLogKey{}).(*requestLog)
	if meta == nil {
		return &requestLog{}
	}
	return meta
}

func clientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
		return forwarded
	}
	host := r.RemoteAddr
	if parsedHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsedHost
	}
	return host
}

func copyRequestHeaders(source http.Header, names []string) http.Header {
	headers := make(http.Header)
	for _, name := range names {
		if value := source.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	return headers
}

func copyResponseHeaders(destination, source http.Header) {
	hopHeaders := map[string]bool{
		"connection": true, "keep-alive": true, "proxy-authenticate": true,
		"proxy-authorization": true, "te": true, "trailer": true,
		"transfer-encoding": true, "upgrade": true,
	}
	for _, value := range source.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			hopHeaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range source {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "access-control-") || lower == "set-cookie" || hopHeaders[lower] {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		origin = "*"
	} else if parsed, err := url.Parse(origin); err != nil || parsed.Scheme != "https" || (parsed.Hostname() != "bilibili.com" && !strings.HasSuffix(parsed.Hostname(), ".bilibili.com")) {
		origin = ""
	}
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range, X-Bili-Cookie, X-Bili-Referer")
	w.Header().Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Length, Content-Range, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
}

func writeTargetError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if err.Error() == "unauthorized" {
		status = http.StatusUnauthorized
	}
	http.Error(w, err.Error(), status)
}
