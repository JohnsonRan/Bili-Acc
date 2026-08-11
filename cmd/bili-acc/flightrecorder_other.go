//go:build !unix

package main

func startFlightRecorder(string) (func(), error) {
	return func() {}, nil
}
