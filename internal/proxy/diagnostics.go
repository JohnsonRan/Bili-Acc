package proxy

import (
	"embed"
	"encoding/json"
	"net/http"
)

//go:embed diagnostics.html
var diagnosticsAssets embed.FS

func (s *server) diagnosticsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		setDiagnosticsHeaders(w)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(s.metrics.snapshot(s.now())); err != nil {
			s.logger.Error("diagnostics response failed", "event", "diagnostics_error", "error_stage", "encode_snapshot")
		}
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		setDiagnosticsHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body, err := diagnosticsAssets.ReadFile("diagnostics.html")
		if err != nil {
			http.Error(w, "Diagnostics unavailable", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setDiagnosticsHeaders(w)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func setDiagnosticsHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
