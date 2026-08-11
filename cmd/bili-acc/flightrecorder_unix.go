//go:build unix

package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/trace"
	"sync"
	"syscall"
	"time"
)

const flightRecorderMaxBytes = 8 << 20

func startFlightRecorder(directory string) (func(), error) {
	if directory == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create trace directory: %w", err)
	}

	recorder := trace.NewFlightRecorder(trace.FlightRecorderConfig{
		MinAge:   5 * time.Second,
		MaxBytes: flightRecorderMaxBytes,
	})
	if err := recorder.Start(); err != nil {
		return nil, fmt.Errorf("start flight recorder: %w", err)
	}

	traceSignals := make(chan os.Signal, 1)
	signal.Notify(traceSignals, syscall.SIGUSR1)
	done := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Go(func() {
		for {
			select {
			case <-traceSignals:
				path, err := writeFlightRecording(directory, recorder)
				if err != nil {
					slog.Error("flight recording failed", "event", "flight_recording_failed", "error", err)
					continue
				}
				slog.Info("flight recording written", "event", "flight_recording_written", "path", path)
			case <-done:
				return
			}
		}
	})

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			signal.Stop(traceSignals)
			close(done)
			watcher.Wait()
			recorder.Stop()
		})
	}, nil
}

func writeFlightRecording(directory string, recorder *trace.FlightRecorder) (path string, err error) {
	file, err := os.CreateTemp(directory, ".runtime-*.trace.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := file.Name()
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(temporaryPath)
		}
	}()

	if _, err = recorder.WriteTo(file); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	path = filepath.Join(directory, "runtime-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".trace")
	if err = os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	return path, nil
}
