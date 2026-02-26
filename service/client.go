package service

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/tonkeeper/tongo/config"
	"github.com/tonkeeper/tongo/liteapi"
)

const (
	configURL = "https://ton.org/global-config.json"
	cacheTTL  = 7 * 24 * time.Hour
)

var globalConfigCache struct {
	sync.Mutex
	conf      *config.GlobalConfigurationFile
	fetchedAt time.Time
}

// NewClientWithCachedConfig downloads the TON global config and caches it in memory for cacheTTL.
func NewClientWithCachedConfig() (*liteapi.Client, error) {
	conf, err := getCachedGlobalConfig()
	if err != nil {
		return nil, err
	}

	maxConns := len(conf.LiteServers)
	log.Printf("connecting to %d liteservers", maxConns)

	return liteapi.NewClient(
		liteapi.WithConfigurationFile(*conf),
		liteapi.WithMaxConnectionsNumber(maxConns),
		liteapi.WithAsyncConnectionsInit(),
	)
}

func getCachedGlobalConfig() (*config.GlobalConfigurationFile, error) {
	globalConfigCache.Lock()
	defer globalConfigCache.Unlock()

	if globalConfigCache.conf != nil && time.Since(globalConfigCache.fetchedAt) < cacheTTL {
		log.Printf("using in-memory cached config (age: %s)", time.Since(globalConfigCache.fetchedAt).Round(time.Minute))
		return globalConfigCache.conf, nil
	}

	log.Printf("downloading config from %s...", configURL)
	resp, err := http.Get(configURL) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("download config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read config body: %w", err)
	}
	conf, err := config.ParseConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	globalConfigCache.conf = conf
	globalConfigCache.fetchedAt = time.Now()
	log.Printf("global config cached in memory")

	return conf, nil
}
