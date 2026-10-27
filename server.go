package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const (
	defaultAuthTimeout = 15 * time.Second
	serverWriteGrace   = 5 * time.Second
)

func newAuthClient(server AuthServer) *authClient {
	return &authClient{
		server: server,
		client: &http.Client{
			Timeout:       time.Duration(server.Timeout) * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func newHTTPServer(cfg *Config) *http.Server {
	authTimeout := time.Duration(cfg.AuthTimeoutSeconds) * time.Second
	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/sessionserver/session/minecraft/hasJoined", hasJoinedHandler(authTimeout))
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      authTimeout + serverWriteGrace,
	}
}

func authenticationStopped(w http.ResponseWriter, requestContext, authContext context.Context) bool {
	if requestContext.Err() != nil {
		return true
	}
	if authContext.Err() != nil {
		http.Error(w, "authentication upstream timeout", http.StatusBadGateway)
		return true
	}
	return false
}
