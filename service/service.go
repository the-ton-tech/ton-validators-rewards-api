package service

import "github.com/tonkeeper/tongo/liteapi"

// Service holds the blockchain client and provides validator statistics methods.
type Service struct {
	client *liteapi.Client
}

// New creates a new Service with the given liteapi client.
func New(client *liteapi.Client) *Service {
	return &Service{client: client}
}
