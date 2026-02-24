package service

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tonkeeper/tongo/config"
	"github.com/tonkeeper/tongo/liteapi"
)

const (
	configURL = "https://ton.org/global-config.json"
	cacheTTL  = 7 * 24 * time.Hour
)

// NewClientWithCachedConfig downloads the TON global config and caches it locally for 7 days.
func NewClientWithCachedConfig() (*liteapi.Client, error) {
	cacheFile := filepath.Join(os.TempDir(), "ton-global-config.json")

	var conf *config.GlobalConfigurationFile

	if info, err := os.Stat(cacheFile); err == nil && time.Since(info.ModTime()) < cacheTTL {
		conf, err = config.ParseConfigFile(cacheFile)
		if err == nil {
			log.Printf("using cached config (age: %s)", time.Since(info.ModTime()).Round(time.Minute))
		}
	}

	if conf == nil {
		log.Printf("downloading config from %s...", configURL)
		resp, err := http.Get(configURL) //nolint:noctx
		if err != nil {
			return nil, fmt.Errorf("download config: %w", err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read config body: %w", err)
		}
		conf, err = config.ParseConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		if err := os.WriteFile(cacheFile, data, 0o644); err != nil {
			log.Printf("warning: could not cache config: %v", err)
		} else {
			log.Printf("config cached to %s", cacheFile)
		}
	}

	nServers := len(conf.LiteServers)
	maxConns := nServers
	if maxConns > 10 {
		maxConns = 10
	}
	log.Printf("connecting to %d/%d liteservers", maxConns, nServers)

	return liteapi.NewClient(
		liteapi.WithConfigurationFile(*conf),
		liteapi.WithMaxConnectionsNumber(maxConns),
		liteapi.WithAsyncConnectionsInit(),
	)
}
