//go:build !darwin && !linux

package credentials

import "fmt"

type stubExtractor struct{}

func (e *stubExtractor) Extract(service string) (string, error) {
	return "", fmt.Errorf("credential extraction not supported on this platform — place credentials file manually")
}

func platformExtractor() tokenExtractor {
	return &stubExtractor{}
}
