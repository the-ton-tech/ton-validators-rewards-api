package service

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
	configPath string
}

// NewClient creates a liteapi client from a config file path or the default remote URL.
func NewClient(configPath string) (*liteapi.Client, error) {
	conf, err := getCachedConfig(configPath)
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

func getCachedConfig(configPath string) (*config.GlobalConfigurationFile, error) {
	globalConfigCache.Lock()
	defer globalConfigCache.Unlock()

	// Invalidate cache if config path changed.
	if globalConfigCache.conf != nil && globalConfigCache.configPath == configPath && time.Since(globalConfigCache.fetchedAt) < cacheTTL {
		log.Printf("using in-memory cached config (age: %s)", time.Since(globalConfigCache.fetchedAt).Round(time.Minute))
		return globalConfigCache.conf, nil
	}

	var conf *config.GlobalConfigurationFile
	var err error

	if configPath != "" {
		log.Printf("loading config from %s...", configPath)
		conf, err = loadConfigFromFile(configPath)
	} else {
		log.Printf("downloading config from %s...", configURL)
		conf, err = downloadConfig()
	}
	if err != nil {
		return nil, err
	}

	globalConfigCache.conf = conf
	globalConfigCache.fetchedAt = time.Now()
	globalConfigCache.configPath = configPath
	log.Printf("config cached in memory (%d liteservers)", len(conf.LiteServers))

	return conf, nil
}

func loadConfigFromFile(path string) (*config.GlobalConfigurationFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	conf, err := config.ParseConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}
	return conf, nil
}

func downloadConfig() (*config.GlobalConfigurationFile, error) {
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
	return conf, nil
}
