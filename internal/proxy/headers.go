package proxy

import (
	"encoding/base64"
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
	case strings.HasPrefix(path, "/proxy/"), strings.HasPrefix(path, "/proxy-group/"):
		return "media"
	case strings.HasPrefix(path, "/media-groups/"):
		return "media_registration"
	case strings.HasPrefix(path, "/playurl-grpc/"):
		return "playurl_grpc"
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

func copyGRPCRequestHeaders(source http.Header) http.Header {
	hopHeaders := map[string]bool{
		"connection": true, "content-length": true, "host": true, "keep-alive": true,
		"proxy-authenticate": true, "proxy-authorization": true, "proxy-connection": true, "trailer": true,
		"transfer-encoding": true, "upgrade": true,
	}
	for _, value := range source.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			hopHeaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	headers := make(http.Header)
	for name, values := range source {
		lower := strings.ToLower(name)
		proxyMetadata := lower == "forwarded" || lower == "via" || lower == "x-real-ip" || lower == "true-client-ip" || lower == "cdn-loop" || strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "cf-")
		if hopHeaders[lower] || proxyMetadata || strings.HasPrefix(lower, "access-control-") || lower == "x-bili-cookie" || lower == "x-bili-referer" || lower == strings.ToLower(grpcOriginalContentTypeHdr) {
			continue
		}
		if lower == "te" {
			if strings.EqualFold(strings.TrimSpace(source.Get(name)), "trailers") {
				headers.Set("Te", "trailers")
			}
			continue
		}
		for _, value := range values {
			headers.Add(name, value)
		}
	}
	return headers
}

func normalizeGRPCMetadata(headers http.Header) {
	for _, name := range []string{"X-Bili-Aurora-Zone", "X-Bili-Exps-Bin"} {
		headers.Del(name)
	}
	// Wi-Fi with no carrier/free-data metadata avoids carrying the client's network classification upstream.
	headers.Set("X-Bili-Network-Bin", base64.StdEncoding.EncodeToString([]byte{0x08, 0x01}))
	if value := headers.Get("X-Bili-Device-Bin"); value != "" {
		if decoded, encoding, ok := decodeBinaryMetadata(value); ok {
			if normalized, ok := removeProtobufField(decoded, 12); ok {
				headers.Set("X-Bili-Device-Bin", encoding.EncodeToString(normalized))
			}
		}
	}
}

func decodeBinaryMetadata(value string) ([]byte, *base64.Encoding, bool) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, encoding, true
		}
	}
	return nil, nil, false
}

func removeProtobufField(message []byte, removeNumber uint64) ([]byte, bool) {
	output := make([]byte, 0, len(message))
	for offset := 0; offset < len(message); {
		start := offset
		tag, next, ok := readProtobufVarint(message, offset)
		if !ok || tag == 0 {
			return nil, false
		}
		offset = next
		wireType := tag & 7
		switch wireType {
		case 0:
			_, offset, ok = readProtobufVarint(message, offset)
		case 1:
			offset += 8
			ok = offset <= len(message)
		case 2:
			var length uint64
			length, offset, ok = readProtobufVarint(message, offset)
			if ok && length <= uint64(len(message)-offset) {
				offset += int(length)
			} else {
				ok = false
			}
		case 5:
			offset += 4
			ok = offset <= len(message)
		default:
			ok = false
		}
		if !ok {
			return nil, false
		}
		if tag>>3 != removeNumber {
			output = append(output, message[start:offset]...)
		}
	}
	return output, true
}

func readProtobufVarint(message []byte, offset int) (uint64, int, bool) {
	var value uint64
	for shift := uint(0); offset < len(message) && shift < 64; shift += 7 {
		current := message[offset]
		offset++
		value |= uint64(current&0x7f) << shift
		if current&0x80 == 0 {
			return value, offset, true
		}
	}
	return 0, offset, false
}

func copyResponseHeaders(destination, source http.Header) {
	hopHeaders := map[string]bool{
		"connection": true, "keep-alive": true, "proxy-authenticate": true,
		"proxy-authorization": true, "proxy-connection": true, "te": true, "trailer": true,
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
		if origin != "*" {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
	}
	w.Header().Set("Vary", "Origin")
	methods := "GET, HEAD, OPTIONS"
	if strings.HasPrefix(r.URL.Path, "/playurl-grpc/") || strings.HasPrefix(r.URL.Path, "/media-groups/") {
		methods = "POST, OPTIONS"
	}
	w.Header().Set("Access-Control-Allow-Methods", methods)
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range, X-Bili-Acc-Grpc-Content-Type, X-Bili-Cookie, X-Bili-Referer")
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
