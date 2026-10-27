package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

const maxDurationSeconds = int64((1<<63 - 1) / time.Second)

type AuthServer struct {
	Priority int    `json:"priority"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Timeout  int    `json:"timeout"`
}

type CacheConfig struct {
	TTLSeconds             int `json:"ttlSeconds"`
	CleanupIntervalSeconds int `json:"cleanupIntervalSeconds"`
	MaxEntries             int `json:"maxEntries"`
}

type Config struct {
	Port               int          `json:"port"`
	AuthTimeoutSeconds int          `json:"authTimeoutSeconds"`
	AuthServers        []AuthServer `json:"authServers"`
	Cache              CacheConfig  `json:"cache"`
}

func defaultCacheConfig() CacheConfig {
	return CacheConfig{
		TTLSeconds:             int(defaultCacheTTL / time.Second),
		CleanupIntervalSeconds: int(defaultCacheCleanupInterval / time.Second),
		MaxEntries:             defaultCacheMaxEntries,
	}
}

func defaultConfig() *Config {
	return &Config{
		Port:               32217,
		AuthTimeoutSeconds: int(defaultAuthTimeout / time.Second),
		Cache:              defaultCacheConfig(),
		AuthServers: []AuthServer{
			{Priority: 0, Name: "Mojang", URL: "https://sessionserver.mojang.com", Timeout: 5},
			{Priority: 1, Name: "LittleSkin", URL: "https://littleskin.cn/api/yggdrasil/sessionserver", Timeout: 5},
		},
	}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := defaultConfig()
			if writeErr := writeDefaultConfig(path, cfg); writeErr != nil {
				return nil, fmt.Errorf("failed to write default config: %w", writeErr)
			}
			fmt.Printf("Config file not found, created default at %s\n", path)
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	cfg := Config{Cache: defaultCacheConfig(), AuthTimeoutSeconds: int(defaultAuthTimeout / time.Second)}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("port must be between 1 and 65535")
	}
	if len(cfg.AuthServers) == 0 {
		return nil, fmt.Errorf("at least one auth server is required")
	}

	for i, server := range cfg.AuthServers {
		if server.Timeout <= 0 || int64(server.Timeout) > maxDurationSeconds {
			return nil, fmt.Errorf("authServers[%d].timeout must be > 0 and <= %d", i, maxDurationSeconds)
		}
		if _, err := parseAuthServerURL(server.URL); err != nil {
			return nil, fmt.Errorf("authServers[%d].url: %w", i, err)
		}
	}
	if cfg.AuthTimeoutSeconds <= 0 || int64(cfg.AuthTimeoutSeconds) > maxDurationSeconds-int64(serverWriteGrace/time.Second) {
		return nil, fmt.Errorf("authTimeoutSeconds must be > 0 and <= %d", maxDurationSeconds-int64(serverWriteGrace/time.Second))
	}

	if cfg.Cache.TTLSeconds <= 0 || int64(cfg.Cache.TTLSeconds) > maxDurationSeconds {
		return nil, fmt.Errorf("cache.ttlSeconds must be > 0 and <= %d", maxDurationSeconds)
	}
	if cfg.Cache.CleanupIntervalSeconds <= 0 || int64(cfg.Cache.CleanupIntervalSeconds) > maxDurationSeconds {
		return nil, fmt.Errorf("cache.cleanupIntervalSeconds must be > 0 and <= %d", maxDurationSeconds)
	}
	if cfg.Cache.MaxEntries <= 0 {
		return nil, fmt.Errorf("cache.maxEntries must be > 0")
	}

	return &cfg, nil
}

func parseAuthServerURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return nil, fmt.Errorf("must be an absolute HTTP(S) URL with a host")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, fmt.Errorf("URL port must be between 1 and 65535")
		}
	}
	return parsed, nil
}

func writeDefaultConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
