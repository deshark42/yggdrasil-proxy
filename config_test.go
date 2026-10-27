package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigCacheDefaultsAndOverrides(t *testing.T) {
	defaults := defaultCacheConfig()
	tests := []struct {
		name, cache string
		want        CacheConfig
	}{
		{"old configuration", "", defaults},
		{"empty cache", `,"cache":{}`, defaults},
		{"partial override", `,"cache":{"maxEntries":7}`, CacheConfig{TTLSeconds: defaults.TTLSeconds, CleanupIntervalSeconds: defaults.CleanupIntervalSeconds, MaxEntries: 7}},
		{"full override", `,"cache":{"ttlSeconds":60,"cleanupIntervalSeconds":5,"maxEntries":2}`, CacheConfig{TTLSeconds: 60, CleanupIntervalSeconds: 5, MaxEntries: 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"port":12345,"authServers":[{"name":"Custom","url":"https://example.com","timeout":2}]` + test.cache + "}"
			cfg, err := LoadConfig(writeTestConfig(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Cache != test.want {
				t.Fatalf("cache = %+v, want %+v", cfg.Cache, test.want)
			}
			if cfg.Port != 12345 || len(cfg.AuthServers) != 1 || cfg.AuthServers[0].Name != "Custom" {
				t.Fatalf("existing settings changed: %+v", cfg)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidCacheSettings(t *testing.T) {
	tests := []struct{ name, cache, wantError string }{
		{"zero TTL", `{"ttlSeconds":0}`, "cache.ttlSeconds"},
		{"negative TTL", `{"ttlSeconds":-1}`, "cache.ttlSeconds"},
		{"overflowing TTL", `{"ttlSeconds":9223372036854775807}`, "ttlSeconds"},
		{"zero cleanup interval", `{"cleanupIntervalSeconds":0}`, "cache.cleanupIntervalSeconds"},
		{"negative cleanup interval", `{"cleanupIntervalSeconds":-1}`, "cache.cleanupIntervalSeconds"},
		{"overflowing cleanup interval", `{"cleanupIntervalSeconds":9223372036854775807}`, "cleanupIntervalSeconds"},
		{"zero capacity", `{"maxEntries":0}`, "cache.maxEntries"},
		{"negative capacity", `{"maxEntries":-1}`, "cache.maxEntries"},
		{"fractional capacity", `{"maxEntries":1.5}`, "maxEntries"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"port":32217,"authServers":[{"url":"https://example.com"}],"cache":` + test.cache + "}"
			if _, err := LoadConfig(writeTestConfig(t, body)); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestLoadConfigCreatesDefaultCacheSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache != defaultCacheConfig() {
		t.Fatalf("cache = %+v, want defaults", cfg.Cache)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, reloaded) {
		t.Fatalf("reloaded = %+v, want %+v", reloaded, cfg)
	}
	example, err := LoadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, example) {
		t.Fatalf("example = %+v, want %+v", example, cfg)
	}
}

func TestLoadConfigStillRequiresExistingSettings(t *testing.T) {
	for _, body := range []string{
		`{"authServers":[{"url":"https://example.com"}]}`,
		`{"port":32217}`,
	} {
		if _, err := LoadConfig(writeTestConfig(t, body)); err == nil {
			t.Fatalf("accepted incomplete config %s", body)
		}
	}
}
