package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
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
	logger, err := newLogger(os.Stderr, os.Getenv("LOG_LEVEL"), os.Getenv("LOG_FORMAT"))
	if err != nil {
		logger, _ = newLogger(os.Stderr, "", "")
		logger.Error("logging_configuration_failed", "error", err.Error())
		os.Exit(1)
	}
	slog.SetDefault(logger)

	cfg, err := LoadConfig("config.json")
	if err != nil {
		slog.Error("configuration_failed", "config_path", "config.json", "error", err.Error())
		os.Exit(1)
	}

	playerCache = newUpstreamCache(cfg.Cache.MaxEntries, time.Duration(cfg.Cache.TTLSeconds)*time.Second)
	cleanupContext, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	go playerCache.runCleanup(cleanupContext, time.Duration(cfg.Cache.CleanupIntervalSeconds)*time.Second)

	sort.Slice(cfg.AuthServers, func(i, j int) bool {
		return cfg.AuthServers[i].Priority < cfg.AuthServers[j].Priority
	})

	for _, s := range cfg.AuthServers {
		authClients = append(authClients, newAuthClient(s))
		attrs := append(upstreamLogFields(s), "priority", s.Priority, "timeout_seconds", s.Timeout)
		slog.Info("upstream_configured", attrs...)
	}

	metaResponse, _ = json.Marshal(map[string]string{
		"name":    "Yggdrasil Proxy Server",
		"version": "1.0",
	})

	server := newHTTPServer(cfg)
	slog.Info("proxy_starting", "address", server.Addr, "upstreams", len(authClients),
		"auth_timeout_seconds", cfg.AuthTimeoutSeconds, "cache_max_entries", cfg.Cache.MaxEntries,
		"cache_ttl_seconds", cfg.Cache.TTLSeconds, "cache_cleanup_interval_seconds", cfg.Cache.CleanupIntervalSeconds)
	if err := server.ListenAndServe(); err != nil {
		slog.Error("server_failed", "address", server.Addr, "error", err.Error())
		os.Exit(1)
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
	status  int
	reason  string
}

func hasJoinedHandler(timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := r.URL.Query().Get("username")
		serverID := r.URL.Query().Get("serverId")
		requestLog := newAuthenticationLog(w, username)
		defer requestLog.finish()

		if username == "" || serverID == "" {
			requestLog.complete("invalid_request", http.StatusBadRequest)
			http.Error(w, "missing username or serverId", http.StatusBadRequest)
			return
		}

		requestContext := r.Context()
		authContext, cancel := context.WithTimeout(requestContext, timeout)
		defer cancel()
		r = r.WithContext(authContext)

		clients := make([]*authClient, 0, len(authClients)+1)
		if cachedURL, ok := playerCache.Load(username); ok {
			if ac := findClient(cachedURL); ac != nil {
				clients = append(clients, ac)
				requestLog.cacheHit = true
			}
		}
		clients = append(clients, authClients...)

		attempted := make(map[string]bool, len(clients))
		hadError := false
		for _, ac := range clients {
			if requestLog.stopped(w, requestContext, authContext) {
				return
			}
			if attempted[ac.server.URL] {
				continue
			}
			attempted[ac.server.URL] = true

			requestLog.attempts++
			attemptStarted := time.Now()
			result := ac.proxyRequest(r, username, serverID)
			requestLog.attempt(authContext, ac.server, result, attemptStarted)
			if requestLog.stopped(w, requestContext, authContext) {
				return
			}
			switch result.outcome {
			case authSuccess:
				requestLog.complete("success", http.StatusOK)
				requestLog.upstream = &ac.server
				playerCache.Store(username, ac.server.URL)
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write(result.body); err != nil {
					requestLog.complete("response_write_error", http.StatusOK)
				}
				return
			case authNoMatch:
				continue
			case upstreamError:
				hadError = true
			}
		}

		if hadError {
			requestLog.complete("upstream_error", http.StatusBadGateway)
			http.Error(w, "authentication upstream error", http.StatusBadGateway)
			return
		}
		requestLog.complete("no_match", http.StatusNoContent)
		w.WriteHeader(http.StatusNoContent)
	}
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
	proxyURL, err := parseAuthServerURL(ac.server.URL)
	if err != nil {
		return authResult{outcome: upstreamError, reason: "invalid_upstream_url"}
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
		return authResult{outcome: upstreamError, reason: "request_build_error"}
	}

	resp, err := ac.client.Do(req)
	if err != nil {
		return authResult{outcome: upstreamError, reason: requestErrorReason(err)}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return authResult{outcome: authNoMatch, status: resp.StatusCode}
	case http.StatusOK:
	default:
		return authResult{outcome: upstreamError, status: resp.StatusCode, reason: "unexpected_status"}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileResponseSize+1))
	if err != nil {
		reason := requestErrorReason(err)
		if reason == "request_error" {
			reason = "response_read_error"
		}
		return authResult{outcome: upstreamError, status: resp.StatusCode, reason: reason}
	}
	if len(body) > maxProfileResponseSize {
		return authResult{outcome: upstreamError, status: resp.StatusCode, reason: "profile_too_large"}
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
		return authResult{outcome: upstreamError, status: resp.StatusCode, reason: "invalid_profile_json"}
	}
	if len(profile.ID) != 32 || strings.TrimSpace(profile.Name) == "" {
		return authResult{outcome: upstreamError, status: resp.StatusCode, reason: "invalid_profile"}
	}
	if _, err := hex.DecodeString(profile.ID); err != nil {
		return authResult{outcome: upstreamError, status: resp.StatusCode, reason: "invalid_profile_uuid"}
	}
	return authResult{outcome: authSuccess, body: body, status: resp.StatusCode}
}
