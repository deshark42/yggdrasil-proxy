package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPServerTimeouts(t *testing.T) {
	cfg := defaultConfig()
	cfg.Port = 12345
	cfg.AuthTimeoutSeconds = 20
	server := newHTTPServer(cfg)
	if server.Addr != ":12345" {
		t.Errorf("address = %q", server.Addr)
	}
	if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 10*time.Second || server.IdleTimeout != 60*time.Second {
		t.Errorf("connection timeouts = header %v, read %v, idle %v", server.ReadHeaderTimeout, server.ReadTimeout, server.IdleTimeout)
	}
	if server.WriteTimeout != 25*time.Second {
		t.Errorf("write timeout = %v, want auth budget plus grace", server.WriteTimeout)
	}
	resetHandlerState(t)
	var deadline time.Time
	client := newAuthClient(AuthServer{URL: "http://upstream.invalid", Timeout: 30})
	if client.client.Timeout != 30*time.Second {
		t.Errorf("upstream timeout = %v, want 30s", client.client.Timeout)
	}
	client.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
		var ok bool
		deadline, ok = r.Context().Deadline()
		if !ok {
			t.Error("upstream context has no deadline")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
	})
	authClients = []*authClient{client}
	start := time.Now()
	got := httptest.NewRecorder()
	server.Handler.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/sessionserver/session/minecraft/hasJoined?username=Player&serverId=hash", nil))
	if got.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got.Code)
	}
	if deadline.Before(start.Add(20*time.Second)) || deadline.After(time.Now().Add(20*time.Second)) {
		t.Errorf("upstream deadline %v does not use configured 20 second budget", deadline)
	}
}

func TestHasJoinedSharedDeadline(t *testing.T) {
	resetHandlerState(t)
	var deadlines []time.Time
	for i := 0; i < 3; i++ {
		client := newAuthClient(AuthServer{URL: fmt.Sprintf("http://upstream%d.invalid", i), Timeout: 5})
		client.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok {
				t.Error("missing authentication deadline")
			}
			deadlines = append(deadlines, deadline)
			return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
		})
		authClients = append(authClients, client)
	}
	got := httptest.NewRecorder()
	hasJoinedHandler(time.Second)(got, httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=hash", nil))
	if got.Code != http.StatusNoContent || len(deadlines) != 3 {
		t.Fatalf("status = %d, attempts = %d", got.Code, len(deadlines))
	}
	for _, deadline := range deadlines[1:] {
		if !deadline.Equal(deadlines[0]) {
			t.Errorf("deadline reset between upstreams: %v", deadlines)
		}
	}
}

func TestHasJoinedBudgetStopsAttempts(t *testing.T) {
	for _, resultAfterDeadline := range []bool{false, true} {
		t.Run(fmt.Sprint(resultAfterDeadline), func(t *testing.T) {
			resetHandlerState(t)
			var calls testCallLog
			first := newAuthClient(AuthServer{URL: "http://slow.invalid", Timeout: 5})
			first.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.add("slow")
				<-r.Context().Done()
				if !resultAfterDeadline {
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(testProfile))}, nil
			})
			authClients = append(authClients, first)
			addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("unexpected fallback")
				io.WriteString(w, testProfile)
			})
			playerCache.Store("Player", first.server.URL)
			got := httptest.NewRecorder()
			hasJoinedHandler(20*time.Millisecond)(got, httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=hash", nil))
			if got.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", got.Code)
			}
			calls.assert(t, "slow")
			assertCachedURL(t, "Player", first.server.URL)
		})
	}
	t.Run("budget expired before first attempt", func(t *testing.T) {
		resetHandlerState(t)
		var calls testCallLog
		addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) { calls.add("unexpected") })
		got := httptest.NewRecorder()
		hasJoinedHandler(-time.Second)(got, httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=hash", nil))
		if got.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", got.Code)
		}
		calls.assert(t)
	})
}

func TestHasJoinedBudgetCancelsNetworkIO(t *testing.T) {
	for _, duringBody := range []bool{false, true} {
		t.Run(fmt.Sprint(duringBody), func(t *testing.T) {
			resetHandlerState(t)
			canceled := make(chan struct{})
			var entered atomic.Bool
			addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				entered.Store(true)
				if duringBody {
					w.Header().Set("Content-Length", fmt.Sprint(len(testProfile)))
					io.WriteString(w, testProfile[:1])
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(canceled)
			})
			got := httptest.NewRecorder()
			hasJoinedHandler(200*time.Millisecond)(got, httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=hash", nil))
			if got.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", got.Code)
			}
			if !entered.Load() {
				t.Fatal("request did not reach upstream before budget expired")
			}
			select {
			case <-canceled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream network request was not canceled")
			}
			if _, ok := playerCache.Load("Player"); ok {
				t.Error("timed out request populated cache")
			}
		})
	}
}

func TestUpstreamRedirectsAreErrors(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, fallbackSuccess := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/fallback=%t", status, fallbackSuccess), func(t *testing.T) {
				resetHandlerState(t)
				var redirected atomic.Int32
				destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); io.WriteString(w, testProfile) }))
				defer destination.Close()
				first := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, status) })
				fallback := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					if fallbackSuccess {
						io.WriteString(w, testProfile)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				})
				playerCache.Store("Player", first.server.URL)
				got := callHasJoined("/hasJoined?username=Player&serverId=hash")
				wantStatus, wantURL := http.StatusBadGateway, first.server.URL
				if fallbackSuccess {
					wantStatus, wantURL = http.StatusOK, fallback.server.URL
				}
				if got.Code != wantStatus {
					t.Errorf("status = %d, want %d", got.Code, wantStatus)
				}
				if redirected.Load() != 0 {
					t.Errorf("redirect followed %d times", redirected.Load())
				}
				assertCachedURL(t, "Player", wantURL)
			})
		}
	}
}

func TestDownstreamDeadlineStillStopsWithoutResponse(t *testing.T) {
	resetHandlerState(t)
	calls := 0
	client := newAuthClient(AuthServer{URL: "http://upstream.invalid", Timeout: 1})
	client.client.Transport = testRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, context.Canceled
	})
	authClients = []*authClient{client}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	got := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=hash", nil).WithContext(ctx)
	hasJoinedHandler(time.Second)(got, request)
	if got.Code != http.StatusOK || got.Body.Len() != 0 {
		t.Errorf("response written after downstream deadline: status %d, body %q", got.Code, got.Body.String())
	}
	if calls != 0 {
		t.Errorf("expired downstream request made %d upstream attempts", calls)
	}
}
