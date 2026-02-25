package service

import (
	"log"
	"sync"
	"time"

	"github.com/tonkeeper/tongo/liteapi"
)

// Service holds the blockchain client and provides validator statistics methods.
type Service struct {
	mu           sync.RWMutex
	client       *liteapi.Client
	clientInitAt time.Time
}

// New creates a new Service with the given liteapi client.
func New(client *liteapi.Client) *Service {
	return &Service{
		client:       client,
		clientInitAt: time.Now(),
	}
}

func (s *Service) currentClient() *liteapi.Client {
	s.mu.RLock()
	client := s.client
	needsRefresh := time.Since(s.clientInitAt) >= cacheTTL
	s.mu.RUnlock()
	if !needsRefresh {
		return client
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Another goroutine could refresh while we were waiting for the write lock.
	if time.Since(s.clientInitAt) >= cacheTTL {
		refreshed, err := NewClientWithCachedConfig()
		if err != nil {
			log.Printf("warning: failed to refresh lite client, keeping current one: %v", err)
			return s.client
		}
		s.client = refreshed
		s.clientInitAt = time.Now()
		log.Printf("lite client refreshed")
	}
	return s.client
}
