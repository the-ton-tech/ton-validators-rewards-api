package service

import (
	"log"
	"strings"
	"time"
)

const (
	retryAttempts = 3
	retryDelay    = 500 * time.Millisecond
)

// isPermanentError returns true for liteserver errors that will never succeed on retry.
func isPermanentError(err error) bool {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "state already gc'd"),
		strings.Contains(msg, "block not found"):
		return true
	default:
		return false
	}
}

// retry calls fn up to retryAttempts times, sleeping retryDelay between attempts.
// The liteapi connection pool rotates between servers on each call,
// so retrying effectively switches to a different liteserver.
// Permanent errors (e.g. GC'd state) are returned immediately without retrying.
func retry[T any](fn func() (T, error)) (T, error) {
	var lastErr error
	for i := range retryAttempts {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if isPermanentError(err) {
			break
		}
		if i < retryAttempts-1 {
			log.Printf("retry %d/%d: %v", i+1, retryAttempts-1, err)
			time.Sleep(retryDelay)
		}
	}
	var zero T
	return zero, lastErr
}
