//go:build unix

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/trace"
	"syscall"
	"testing"
	"time"
)

func TestStartFlightRecorderDisabled(t *testing.T) {
	stop, err := startFlightRecorder("")
	if err != nil {
		t.Fatal(err)
	}
	stop()
	stop()
}

func TestStartFlightRecorderCreatesDirectoryAndStopsTwice(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "traces")
	stop, err := startFlightRecorder(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("trace directory: info=%v err=%v", info, err)
	}
	stop()
	stop()
}

func TestWriteFlightRecording(t *testing.T) {
	recorder := trace.NewFlightRecorder(trace.FlightRecorderConfig{MaxBytes: 1 << 20})
	if err := recorder.Start(); err != nil {
		t.Fatal(err)
	}
	defer recorder.Stop()

	directory := t.TempDir()
	path, err := writeFlightRecording(directory, recorder)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("flight recording is empty")
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("flight recording mode = %o, want 600", got)
	}
	assertNoTemporaryTraces(t, directory)
}

func TestWriteFlightRecordingRemovesTemporaryFileOnFailure(t *testing.T) {
	directory := t.TempDir()
	if _, err := writeFlightRecording(directory, trace.NewFlightRecorder(trace.FlightRecorderConfig{})); err == nil {
		t.Fatal("write with inactive recorder unexpectedly succeeded")
	}
	assertNoTemporaryTraces(t, directory)
}

func TestFlightRecorderSIGUSR1(t *testing.T) {
	if os.Getenv("BILI_ACC_FLIGHT_RECORDER_HELPER") == "1" {
		runFlightRecorderHelper(t)
		return
	}

	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestFlightRecorderSIGUSR1$")
	command.Env = append(os.Environ(),
		"BILI_ACC_FLIGHT_RECORDER_HELPER=1",
		"TRACE_DIR="+directory,
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()

	waitForFile(t, filepath.Join(directory, "helper-ready"), &output)
	if err := command.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	tracePath := waitForTrace(t, directory, &output)
	if info, err := os.Stat(tracePath); err != nil || info.Size() == 0 {
		t.Fatalf("trace file: info=%v err=%v output=%s", info, err, output.String())
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper failed: %v output=%s", err, output.String())
	}
}

func runFlightRecorderHelper(t *testing.T) {
	directory := os.Getenv("TRACE_DIR")
	stop, err := startFlightRecorder(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := os.WriteFile(filepath.Join(directory, "helper-ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
}

func waitForFile(t *testing.T, path string, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; output=%s", path, output.String())
}

func waitForTrace(t *testing.T, directory string, output *bytes.Buffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, err := filepath.Glob(filepath.Join(directory, "runtime-*.trace"))
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 0 {
			return paths[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for flight recording; output=%s", output.String())
	return ""
}

func assertNoTemporaryTraces(t *testing.T, directory string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, ".runtime-*.trace.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("temporary traces remain: %v", paths)
	}
}
