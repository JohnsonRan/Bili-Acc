package main

import (
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentParsersAndDurationBounds(t *testing.T) {
	t.Setenv("TEST_BOOL", "true")
	if value, err := envBool("TEST_BOOL", false); err != nil || !value {
		t.Fatalf("bool value=%t err=%v", value, err)
	}
	t.Setenv("TEST_BOOL", "invalid")
	if _, err := envBool("TEST_BOOL", false); err == nil {
		t.Fatal("invalid bool accepted")
	}

	for _, test := range []struct {
		value   string
		minimum time.Duration
		valid   bool
	}{
		{"0", 10 * time.Second, true},
		{"10s", 10 * time.Second, true},
		{"9.999s", 10 * time.Second, false},
		{"1s", time.Second, true},
		{"999ms", time.Second, false},
		{"-1s", time.Second, false},
		{"invalid", time.Second, false},
	} {
		t.Setenv("TEST_DURATION", test.value)
		_, err := envBoundedDuration("TEST_DURATION", time.Minute, test.minimum)
		if (err == nil) != test.valid {
			t.Fatalf("value=%q minimum=%s valid=%t err=%v", test.value, test.minimum, test.valid, err)
		}
	}

	t.Setenv("TEST_CHOICE", "MASKED")
	if value, err := envChoice("TEST_CHOICE", "masked", "full", "masked", "off"); err != nil || value != "masked" {
		t.Fatalf("choice value=%q err=%v", value, err)
	}
	t.Setenv("TEST_CHOICE", "public")
	if _, err := envChoice("TEST_CHOICE", "masked", "full", "masked", "off"); err == nil {
		t.Fatal("invalid choice accepted")
	}

	for _, value := range []string{"ipv4", "ipv6", "auto"} {
		t.Setenv("TEST_NETWORK", value)
		if got, err := envChoice("TEST_NETWORK", "ipv4", "ipv4", "ipv6", "auto"); err != nil || got != value {
			t.Fatalf("network value=%q got=%q err=%v", value, got, err)
		}
	}
}

func TestLoggerConfigurationUsesExplicitLevels(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		for _, level := range []string{"debug", "info", "warn", "error"} {
			if _, err := newLogger(format, level); err != nil {
				t.Fatalf("format=%s level=%s: %v", format, level, err)
			}
		}
	}
	for _, level := range []string{"verbose", "info+1", "-4"} {
		if _, err := newLogger("text", level); err == nil {
			t.Fatalf("invalid level %q accepted", level)
		}
	}
	if _, err := newLogger("xml", "info"); err == nil {
		t.Fatal("invalid format accepted")
	}
}

func TestBindServersClosesEarlierListenersOnPeerFailure(t *testing.T) {
	firstProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	firstAddress := firstProbe.Addr().String()
	_ = firstProbe.Close()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	servers := []namedServer{
		{name: "proxy", server: &http.Server{Addr: firstAddress}},
		{name: "diagnostics", server: &http.Server{Addr: occupied.Addr().String()}},
	}
	if _, err := bindServers(servers); err == nil {
		t.Fatal("peer bind failure was accepted")
	}
	rebound, err := net.Listen("tcp", firstAddress)
	if err != nil {
		t.Fatalf("first listener leaked after peer failure: %v", err)
	}
	_ = rebound.Close()
}

func TestComposeKeepsSingleProxyPortOnLoopback(t *testing.T) {
	body, err := os.ReadFile("../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		"LOG_CLIENT_IP: \"${LOG_CLIENT_IP:-masked}\"",
		"UPSTREAM_NETWORK: \"${UPSTREAM_NETWORK:-ipv4}\"",
		"MEDIA_IDLE_TIMEOUT: \"${MEDIA_IDLE_TIMEOUT:-20s}\"",
		"127.0.0.1:8080:8080",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("compose missing %q", expected)
		}
	}
	if strings.Contains(text, "ADMIN_LISTEN_ADDR") || strings.Contains(text, "8965:9090") {
		t.Fatal("compose still declares a separate diagnostics listener")
	}
	if strings.Contains(text, "0.0.0.0:") {
		t.Fatal("compose exposes a service publicly")
	}
}

func TestBindServersBindsAllBeforeServe(t *testing.T) {
	servers := []namedServer{
		{name: "proxy", server: &http.Server{Addr: "127.0.0.1:0"}},
		{name: "diagnostics", server: &http.Server{Addr: "127.0.0.1:0"}},
	}
	bound, err := bindServers(servers)
	if err != nil {
		t.Fatal(err)
	}
	if len(bound) != 2 || bound[0].listener.Addr() == nil || bound[1].listener.Addr() == nil {
		t.Fatalf("bound = %+v", bound)
	}
	for _, item := range bound {
		_ = item.listener.Close()
	}
}
