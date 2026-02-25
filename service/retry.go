package service

import (
	"log"
	"time"
)

const (
	retryAttempts = 3
	retryDelay    = 500 * time.Millisecond
)

// retry calls fn up to retryAttempts times, sleeping retryDelay between attempts.
// The liteapi connection pool rotates between servers on each call,
// so retrying effectively switches to a different liteserver.
func retry[T any](fn func() (T, error)) (T, error) {
	var lastErr error
	for i := range retryAttempts {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if i < retryAttempts-1 {
			log.Printf("retry %d/%d: %v", i+1, retryAttempts-1, err)
			time.Sleep(retryDelay)
		}
	}
	var zero T
	return zero, lastErr
}
