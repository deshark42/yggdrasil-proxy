package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
			if cfg.AuthTimeoutSeconds != int(defaultAuthTimeout/time.Second) {
				t.Fatalf("authTimeoutSeconds = %d, want default", cfg.AuthTimeoutSeconds)
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
			body := `{"port":32217,"authServers":[{"url":"https://example.com","timeout":5}],"cache":` + test.cache + "}"
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
		`{"authServers":[{"url":"https://example.com","timeout":5}]}`,
		`{"port":32217}`,
	} {
		if _, err := LoadConfig(writeTestConfig(t, body)); err == nil {
			t.Fatalf("accepted incomplete config %s", body)
		}
	}
}

func TestLoadConfigRejectsInvalidHTTPSettings(t *testing.T) {
	encoded, err := json.Marshal(defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, old, replacement, wantError string }{
		{"zero port", "\"port\":32217", "\"port\":0", "port"},
		{"negative port", "\"port\":32217", "\"port\":-1", "port"},
		{"oversized port", "\"port\":32217", "\"port\":65536", "port"},
		{"zero auth budget", "\"authTimeoutSeconds\":15", "\"authTimeoutSeconds\":0", "authTimeoutSeconds"},
		{"negative auth budget", "\"authTimeoutSeconds\":15", "\"authTimeoutSeconds\":-1", "authTimeoutSeconds"},
		{"overflowing auth budget", "\"authTimeoutSeconds\":15", "\"authTimeoutSeconds\":9223372036854775807", "authTimeoutSeconds"},
		{"overflowing write grace", "\"authTimeoutSeconds\":15", "\"authTimeoutSeconds\":9223372036", "authTimeoutSeconds"},
		{"zero upstream timeout", "\"timeout\":5", "\"timeout\":0", "authServers[0].timeout"},
		{"negative upstream timeout", "\"timeout\":5", "\"timeout\":-1", "authServers[0].timeout"},
		{"missing upstream timeout", ",\"timeout\":5", "", "authServers[0].timeout"},
		{"overflowing upstream timeout", "\"timeout\":5", "\"timeout\":9223372036854775807", "timeout"},
		{"invalid upstream ", "\"url\":\"https://sessionserver.mojang.com\"", "\"url\":\"\"", "authServers[0].url"},
		{"invalid upstream example.com", "\"url\":\"https://sessionserver.mojang.com\"", "\"url\":\"example.com\"", "authServers[0].url"},
		{"invalid upstream ftp://example.com", "\"url\":\"https://sessionserver.mojang.com\"", "\"url\":\"ftp://example.com\"", "authServers[0].url"},
		{"invalid upstream https://", "\"url\":\"https://sessionserver.mojang.com\"", "\"url\":\"https://\"", "authServers[0].url"},
		{"invalid upstream /relative", "\"url\":\"https://sessionserver.mojang.com\"", "\"url\":\"/relative\"", "authServers[0].url"},
		{"invalid upstream http://%invalid", "\"url\":\"https://sessionserver.mojang.com\"", "\"url\":\"http://%invalid\"", "authServers[0].url"},
		{"invalid upstream https://example.com:65536", "\"url\":\"https://sessionserver.mojang.com\"", "\"url\":\"https://example.com:65536\"", "authServers[0].url"},
		{"invalid upstream https://example.com:0", "\"url\":\"https://sessionserver.mojang.com\"", "\"url\":\"https://example.com:0\"", "authServers[0].url"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := strings.Replace(string(encoded), test.old, test.replacement, 1)
			if body == string(encoded) {
				t.Fatal("test did not change configuration")
			}
			if _, err := LoadConfig(writeTestConfig(t, body)); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestLoadConfigHTTPOverridesAndBoundaries(t *testing.T) {
	for _, port := range []int{1, 65535} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Port = port
			cfg.AuthTimeoutSeconds = 30
			cfg.AuthServers = []AuthServer{{URL: "http://[::1]:12345/api?source=proxy", Timeout: 1}}
			body, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(writeTestConfig(t, string(body)))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded, cfg) {
				t.Fatalf("config = %+v, want %+v", loaded, cfg)
			}
		})
	}
}
