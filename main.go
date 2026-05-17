package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

var (
	playerCache  sync.Map
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

func hasJoinedHandler(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	serverID := r.URL.Query().Get("serverId")

	if username == "" || serverID == "" {
		http.Error(w, "missing username or serverId", http.StatusBadRequest)
		return
	}

	// 尝试从缓存读取
	if cachedURL, ok := playerCache.Load(username); ok {
		if ac := findClient(cachedURL.(string)); ac != nil {
			if ac.proxyRequest(w, r, username, serverID) {
				return
			}
			log.Printf("Cached server %s failed for %s, removing cache", cachedURL, username)
			playerCache.Delete(username)
		}
	}

	// 按优先级遍历所有服务器
	for _, ac := range authClients {
		if ac.proxyRequest(w, r, username, serverID) {
			playerCache.Store(username, ac.server.URL)
			return
		}
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

func (ac *authClient) proxyRequest(w http.ResponseWriter, r *http.Request, username, serverID string) bool {
	proxyURL := fmt.Sprintf("%s/session/minecraft/hasJoined?username=%s&serverId=%s",
		ac.server.URL, username, serverID)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, proxyURL, nil)
	if err != nil {
		log.Printf("Error creating request for %s: %v", ac.server.Name, err)
		return false
	}

	resp, err := ac.client.Do(req)
	if err != nil {
		log.Printf("Request to %s failed: %v", ac.server.Name, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) == 0 {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
	return true
}
