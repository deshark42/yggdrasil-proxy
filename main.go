package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

var (
	playerCache  = newUpstreamCache(defaultCacheMaxEntries, defaultCacheTTL)
	authClients  []*authClient
	metaResponse []byte
)

type authClient struct {
	server AuthServer
	client *http.Client
}

func main() {
	cfg, err := LoadConfig("config.json")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	playerCache = newUpstreamCache(cfg.Cache.MaxEntries, time.Duration(cfg.Cache.TTLSeconds)*time.Second)
	cleanupContext, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	go playerCache.runCleanup(cleanupContext, time.Duration(cfg.Cache.CleanupIntervalSeconds)*time.Second)

	sort.Slice(cfg.AuthServers, func(i, j int) bool {
		return cfg.AuthServers[i].Priority < cfg.AuthServers[j].Priority
	})

	for _, s := range cfg.AuthServers {
		authClients = append(authClients, &authClient{
			server: s,
			client: &http.Client{
				Timeout: time.Duration(s.Timeout) * time.Second,
			},
		})
	}

	metaResponse, _ = json.Marshal(map[string]string{
		"name":    "Yggdrasil Proxy Server",
		"version": "1.0",
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/sessionserver/session/minecraft/hasJoined", hasJoinedHandler)

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("Starting Yggdrasil Proxy on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(metaResponse)
}

type authOutcome int

const (
	upstreamError authOutcome = iota
	authSuccess
	authNoMatch
)

const maxProfileResponseSize = 1 << 20

type authResult struct {
	outcome authOutcome
	body    []byte
	err     error
}

func hasJoinedHandler(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	serverID := r.URL.Query().Get("serverId")

	if username == "" || serverID == "" {
		http.Error(w, "missing username or serverId", http.StatusBadRequest)
		return
	}

	clients := make([]*authClient, 0, len(authClients)+1)
	if cachedURL, ok := playerCache.Load(username); ok {
		if ac := findClient(cachedURL); ac != nil {
			clients = append(clients, ac)
		}
	}
	clients = append(clients, authClients...)

	attempted := make(map[string]bool, len(clients))
	hadError := false
	for _, ac := range clients {
		if r.Context().Err() != nil {
			return
		}
		if attempted[ac.server.URL] {
			continue
		}
		attempted[ac.server.URL] = true

		result := ac.proxyRequest(r, username, serverID)
		if r.Context().Err() != nil {
			return
		}
		switch result.outcome {
		case authSuccess:
			playerCache.Store(username, ac.server.URL)
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(result.body); err != nil {
				log.Printf("Failed to write authentication response: %v", err)
			}
			return
		case authNoMatch:
			continue
		case upstreamError:
			hadError = true
			log.Printf("Upstream %s failed: %v", ac.server.Name, result.err)
		}
	}

	if hadError {
		http.Error(w, "authentication upstream error", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func findClient(url string) *authClient {
	for _, ac := range authClients {
		if ac.server.URL == url {
			return ac
		}
	}
	return nil
}

func (ac *authClient) proxyRequest(r *http.Request, username, serverID string) authResult {
	proxyURL, err := url.Parse(ac.server.URL)
	if err != nil || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") || proxyURL.Host == "" {
		return authResult{outcome: upstreamError, err: fmt.Errorf("invalid upstream URL")}
	}
	proxyURL.Path = strings.TrimRight(proxyURL.Path, "/") + "/session/minecraft/hasJoined"
	proxyURL.RawPath = ""
	query := proxyURL.Query()
	query.Set("username", username)
	query.Set("serverId", serverID)
	if ip := r.URL.Query().Get("ip"); ip != "" {
		query.Set("ip", ip)
	} else {
		query.Del("ip")
	}
	proxyURL.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, proxyURL.String(), nil)
	if err != nil {
		return authResult{outcome: upstreamError, err: fmt.Errorf("create request: %w", err)}
	}

	resp, err := ac.client.Do(req)
	if err != nil {
		return authResult{outcome: upstreamError, err: fmt.Errorf("request failed: %w", err)}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return authResult{outcome: authNoMatch}
	case http.StatusOK:
	default:
		return authResult{outcome: upstreamError, err: fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileResponseSize+1))
	if err != nil {
		return authResult{outcome: upstreamError, err: fmt.Errorf("read profile: %w", err)}
	}
	if len(body) > maxProfileResponseSize {
		return authResult{outcome: upstreamError, err: fmt.Errorf("profile response exceeds %d bytes", maxProfileResponseSize)}
	}
	var profile struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Properties []struct {
			Name      string `json:"name"`
			Value     string `json:"value"`
			Signature string `json:"signature"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		return authResult{outcome: upstreamError, err: fmt.Errorf("invalid profile JSON: %w", err)}
	}
	if len(profile.ID) != 32 || strings.TrimSpace(profile.Name) == "" {
		return authResult{outcome: upstreamError, err: fmt.Errorf("profile must contain a UUID and name")}
	}
	if _, err := hex.DecodeString(profile.ID); err != nil {
		return authResult{outcome: upstreamError, err: fmt.Errorf("invalid profile UUID: %w", err)}
	}
	return authResult{outcome: authSuccess, body: body}
}
