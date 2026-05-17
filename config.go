package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type AuthServer struct {
	Priority int    `json:"priority"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Timeout  int    `json:"timeout"`
}

type Config struct {
	Port        int          `json:"port"`
	AuthServers []AuthServer `json:"authServers"`
}

func defaultConfig() *Config {
	return &Config{
		Port: 32217,
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

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if cfg.Port <= 0 {
		return nil, fmt.Errorf("port must be > 0")
	}
	if len(cfg.AuthServers) == 0 {
		return nil, fmt.Errorf("at least one auth server is required")
	}

	return &cfg, nil
}

func writeDefaultConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
