package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/JohnsonRan/Bili-Acc/internal/proxy"
)

type namedServer struct {
	name   string
	server *http.Server
}

type boundServer struct {
	namedServer
	listener net.Listener
}

type serverResult struct {
	name string
	err  error
}

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "event", "server_error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	levelName, err := envChoice("LOG_LEVEL", "info", "debug", "info", "warn", "error")
	if err != nil {
		return err
	}
	logger, err := newLogger(envOrDefault("LOG_FORMAT", "text"), levelName)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	token := os.Getenv("TOKEN")
	if token == "" {
		return errors.New("TOKEN is required")
	}
	listen := envOrDefault("LISTEN_ADDR", ":8080")
	logMediaSuccess, err := envBool("LOG_MEDIA_SUCCESS", false)
	if err != nil {
		return err
	}
	summaryInterval, err := envBoundedDuration("LOG_SUMMARY_INTERVAL", time.Minute, 10*time.Second)
	if err != nil {
		return err
	}
	dedupInterval, err := envBoundedDuration("LOG_ERROR_DEDUP_INTERVAL", 10*time.Second, time.Second)
	if err != nil {
		return err
	}
	clientIPMode, err := envChoice("LOG_CLIENT_IP", "masked", "full", "masked", "off")
	if err != nil {
		return err
	}
	upstreamNetwork, err := envChoice("UPSTREAM_NETWORK", "ipv4", "ipv4", "ipv6", "auto")
	if err != nil {
		return err
	}
	mediaIdleTimeout, err := envBoundedDuration("MEDIA_IDLE_TIMEOUT", 20*time.Second, 5*time.Second)
	if err != nil {
		return err
	}

	stopFlightRecorder, err := startFlightRecorder(os.Getenv("TRACE_DIR"))
	if err != nil {
		return err
	}
	defer stopFlightRecorder()

	app := proxy.NewApplication(token, os.Getenv("PUBLIC_URL"), os.Getenv("ALLOWED_HOSTS"), proxy.Options{
		Logger:             logger,
		LogMediaSuccess:    logMediaSuccess,
		ErrorDedupInterval: dedupInterval,
		ClientIPMode:       clientIPMode,
		UpstreamNetwork:    upstreamNetwork,
		MediaIdleTimeout:   mediaIdleTimeout,
	})
	servers := []namedServer{{
		name: "proxy",
		server: &http.Server{
			Addr:              listen,
			Handler:           app.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		},
	}}

	bound, err := bindServers(servers)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if summaryInterval > 0 {
		go app.RunSummaries(ctx, summaryInterval)
	}

	errCh := make(chan serverResult, len(bound))
	for _, item := range bound {
		item := item
		go func() {
			errCh <- serverResult{name: item.name, err: item.server.Serve(item.listener)}
		}()
		logger.Info("listener started", "event", "listener_started", "listener", item.name, "address", item.listener.Addr().String())
	}

	var runErr error
	select {
	case result := <-errCh:
		if !errors.Is(result.err, http.ErrServerClosed) {
			runErr = fmt.Errorf("%s listener: %w", result.name, result.err)
		}
	case <-ctx.Done():
		logger.Info("shutdown requested", "event", "shutdown_requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, item := range bound {
		if err := item.server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown failed", "event", "shutdown_failed", "listener", item.name, "error", err)
			if closeErr := item.server.Close(); closeErr != nil {
				logger.Warn("forced shutdown failed", "event", "shutdown_close_failed", "listener", item.name, "error", closeErr)
			}
			if runErr == nil {
				runErr = fmt.Errorf("shutdown %s listener: %w", item.name, err)
			}
		}
	}
	return runErr
}

func bindServers(servers []namedServer) ([]boundServer, error) {
	bound := make([]boundServer, 0, len(servers))
	for _, item := range servers {
		listener, err := net.Listen("tcp", item.server.Addr)
		if err != nil {
			for _, opened := range bound {
				_ = opened.listener.Close()
			}
			return nil, fmt.Errorf("bind %s listener: %w", item.name, err)
		}
		bound = append(bound, boundServer{namedServer: item, listener: listener})
	}
	return bound, nil
}

func newLogger(format, levelName string) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(strings.TrimSpace(levelName)) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid LOG_LEVEL %q", levelName)
	}
	options := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, options)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, options)), nil
	default:
		return nil, fmt.Errorf("invalid LOG_FORMAT %q", format)
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid %s: %w", name, err)
	}
	return parsed, nil
}

func envChoice(name, fallback string, allowed ...string) (string, error) {
	value := strings.ToLower(envOrDefault(name, fallback))
	for _, candidate := range allowed {
		if value == candidate {
			return value, nil
		}
	}
	return "", fmt.Errorf("invalid %s %q", name, value)
}

func envBoundedDuration(name string, fallback, minimum time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	if value == "0" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < minimum {
		return 0, fmt.Errorf("invalid %s %q: must be 0 or at least %s", name, value, minimum)
	}
	return parsed, nil
}
