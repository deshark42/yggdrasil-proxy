package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const testProfile = `{"id":"0123456789abcdef0123456789abcdef","name":"Player","properties":[{"name":"textures","value":"opaque+/=","signature":"signed+/="}]}`

func resetHandlerState(t *testing.T) {
	t.Helper()
	authClients = nil
	playerCache = newUpstreamCache(defaultCacheMaxEntries, defaultCacheTTL)
	t.Cleanup(func() {
		authClients = nil
		playerCache = newUpstreamCache(defaultCacheMaxEntries, defaultCacheTTL)
	})
}

func addTestUpstream(t *testing.T, handler http.HandlerFunc) *authClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := &authClient{server: AuthServer{URL: server.URL}, client: server.Client()}
	authClients = append(authClients, client)
	return client
}

func callHasJoined(target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	hasJoinedHandler(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func assertCachedURL(t *testing.T, username, want string) {
	t.Helper()
	got, ok := playerCache.Load(username)
	if !ok || got != want {
		t.Fatalf("cache[%q] = (%v, %v), want %q", username, got, ok, want)
	}
}

type testCallLog struct {
	mu    sync.Mutex
	names []string
}

func (log *testCallLog) add(name string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.names = append(log.names, name)
}

func (log *testCallLog) assert(t *testing.T, want ...string) {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	if !reflect.DeepEqual(log.names, want) {
		t.Fatalf("upstream calls = %v, want %v", log.names, want)
	}
}

func TestHasJoinedStatusAggregation(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int
		want     int
		calls    int
	}{
		{"all no match", []int{204, 204}, 204, 2},
		{"error then no match", []int{500, 204}, 502, 2},
		{"no match then error", []int{204, 503}, 502, 2},
		{"all errors", []int{403, 500}, 502, 2},
		{"unauthorized is error", []int{401}, 502, 1},
		{"not found is error", []int{404}, 502, 1},
		{"rate limit is error", []int{429}, 502, 1},
		{"other success status is error", []int{201}, 502, 1},
		{"redirect without location is error", []int{302}, 502, 1},
		{"success first", []int{200, 500}, 200, 1},
		{"success after error", []int{500, 200}, 200, 2},
		{"success after no match", []int{204, 200}, 200, 2},
		{"success after mixed failures", []int{503, 204, 200, 500}, 200, 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetHandlerState(t)
			var calls testCallLog
			var wantCalls []string
			for i, status := range test.statuses {
				status := status
				name := strings.Repeat("x", i+1)
				if i < test.calls {
					wantCalls = append(wantCalls, name)
				}
				addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					calls.add(name)
					w.WriteHeader(status)
					if status != http.StatusNoContent {
						_, _ = io.WriteString(w, testProfile)
					}
				})
			}
			got := callHasJoined("/sessionserver/session/minecraft/hasJoined?username=Player&serverId=hash")
			if got.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %q", got.Code, test.want, got.Body.String())
			}
			calls.assert(t, wantCalls...)
			if test.want == http.StatusOK {
				if got.Body.String() != testProfile {
					t.Fatalf("body = %q, want original profile %q", got.Body.String(), testProfile)
				}
				assertCachedURL(t, "Player", authClients[test.calls-1].server.URL)
			} else if _, ok := playerCache.Load("Player"); ok {
				t.Fatal("unsuccessful request populated cache")
			}
			if test.want == http.StatusNoContent && got.Body.Len() != 0 {
				t.Fatalf("204 response has body %q", got.Body.String())
			}
		})
	}
}

func TestHasJoinedProfileValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace", " \n\t"},
		{"null", "null"},
		{"empty object", `{}`},
		{"array", `[]`},
		{"string", `"profile"`},
		{"malformed", `{"id":`},
		{"missing id", `{"name":"Player"}`},
		{"empty id", `{"id":"","name":"Player"}`},
		{"short id", `{"id":"0123456789abcdef","name":"Player"}`},
		{"long id", `{"id":"0123456789abcdef0123456789abcdef0","name":"Player"}`},
		{"nonhex id", `{"id":"g123456789abcdef0123456789abcdef","name":"Player"}`},
		{"hyphenated id", `{"id":"01234567-89ab-cdef-0123-456789abcdef","name":"Player"}`},
		{"numeric id", `{"id":123,"name":"Player"}`},
		{"null id", `{"id":null,"name":"Player"}`},
		{"missing name", `{"id":"0123456789abcdef0123456789abcdef"}`},
		{"empty name", `{"id":"0123456789abcdef0123456789abcdef","name":""}`},
		{"null name", `{"id":"0123456789abcdef0123456789abcdef","name":null}`},
		{"numeric name", `{"id":"0123456789abcdef0123456789abcdef","name":42}`},
		{"trailing garbage", testProfile + "garbage"},
		{"second JSON value", testProfile + testProfile},
		{"blank name", `{"id":"0123456789abcdef0123456789abcdef","name":" \t"}`},
		{"invalid properties", `{"id":"0123456789abcdef0123456789abcdef","name":"Player","properties":{}}`},
		{"invalid property value", `{"id":"0123456789abcdef0123456789abcdef","name":"Player","properties":[{"name":"textures","value":42}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetHandlerState(t)
			addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			})
			got := callHasJoined("/hasJoined?username=Player&serverId=hash")
			if got.Code != http.StatusBadGateway {
				t.Fatalf("invalid profile status = %d, want 502; body = %q", got.Code, got.Body.String())
			}
			if _, ok := playerCache.Load("Player"); ok {
				t.Fatal("invalid profile populated cache")
			}
		})
	}
	t.Run("invalid profile falls back", func(t *testing.T) {
		resetHandlerState(t)
		addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{}`)
		})
		winner := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, testProfile)
		})
		got := callHasJoined("/hasJoined?username=Player&serverId=hash")
		if got.Code != http.StatusOK || got.Body.String() != testProfile {
			t.Fatalf("fallback response = %d %q, want original successful profile", got.Code, got.Body.String())
		}
		assertCachedURL(t, "Player", winner.server.URL)
	})
}

func TestHasJoinedPreservesProfileBytes(t *testing.T) {
	for _, body := range []string{
		testProfile,
		" \n" + `{"name":"OtherName","id":"ABCDEF0123456789ABCDEF0123456789","properties":[{"signature":"s+/=","value":"v+/=","name":"textures"}],"extra":{"keep":true}}` + "\n",
	} {
		t.Run(body, func(t *testing.T) {
			resetHandlerState(t)
			winner := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			got := callHasJoined("/hasJoined?username=Player&serverId=hash")
			if got.Code != http.StatusOK || got.Body.String() != body {
				t.Fatalf("response = %d %q, want 200 %q", got.Code, got.Body.String(), body)
			}
			assertCachedURL(t, "Player", winner.server.URL)
		})
	}
}

func TestHasJoinedCacheFirstAndDeduplication(t *testing.T) {
	for _, cachedStatus := range []int{http.StatusNoContent, http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(http.StatusText(cachedStatus), func(t *testing.T) {
			resetHandlerState(t)
			var calls testCallLog
			first := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("priority")
				w.WriteHeader(http.StatusNoContent)
			})
			cached := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("cached")
				w.WriteHeader(cachedStatus)
				if cachedStatus == http.StatusOK {
					_, _ = io.WriteString(w, testProfile)
				}
			})
			last := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("last")
				w.WriteHeader(http.StatusNoContent)
			})
			duplicate := &authClient{server: cached.server, client: cached.client}
			authClients = []*authClient{first, cached, duplicate, first, last, last}
			playerCache.Store("Player", cached.server.URL)
			got := callHasJoined("/hasJoined?username=Player&serverId=hash")
			want := http.StatusNoContent
			if cachedStatus == http.StatusOK {
				want = http.StatusOK
				calls.assert(t, "cached")
			} else {
				if cachedStatus != http.StatusNoContent {
					want = http.StatusBadGateway
				}
				calls.assert(t, "cached", "priority", "last")
			}
			if got.Code != want {
				t.Fatalf("status = %d, want %d", got.Code, want)
			}
			assertCachedURL(t, "Player", cached.server.URL)
		})
	}
}

func TestHasJoinedCacheSurvivalAndReplacement(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		for _, success := range []bool{false, true} {
			name := http.StatusText(status) + "/failure"
			if success {
				name = http.StatusText(status) + "/replacement"
			}
			t.Run(name, func(t *testing.T) {
				resetHandlerState(t)
				cached := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
				})
				fallback := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					if success {
						_, _ = io.WriteString(w, testProfile)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				})
				playerCache.Store("Player", cached.server.URL)
				playerCache.Store("Unrelated", "unchanged")
				got := callHasJoined("/hasJoined?username=Player&serverId=hash")
				wantURL := cached.server.URL
				wantStatus := http.StatusNoContent
				if success {
					wantURL = fallback.server.URL
					wantStatus = http.StatusOK
				} else if status != http.StatusNoContent {
					wantStatus = http.StatusBadGateway
				}
				if got.Code != wantStatus {
					t.Fatalf("status = %d, want %d", got.Code, wantStatus)
				}
				assertCachedURL(t, "Player", wantURL)
				assertCachedURL(t, "Unrelated", "unchanged")
			})
		}
	}
	t.Run("unconfigured cached URL survives failed lookup", func(t *testing.T) {
		resetHandlerState(t)
		playerCache.Store("Player", "https://old.invalid/base")
		addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		got := callHasJoined("/hasJoined?username=Player&serverId=hash")
		if got.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", got.Code)
		}
		assertCachedURL(t, "Player", "https://old.invalid/base")
	})
}

func TestHasJoinedForwardedURL(t *testing.T) {
	for _, basePath := range []string{"", "/", "/api/yggdrasil/sessionserver", "/api/yggdrasil/sessionserver/"} {
		for _, ip := range []string{"", "2001:db8::1", "fe80::1%eth0"} {
			t.Run(basePath+"/"+ip, func(t *testing.T) {
				resetHandlerState(t)
				username := "Player +&=?/雪"
				serverID := "hash +&=?/#%"
				seen := make(chan *url.URL, 1)
				client := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					copyURL := *r.URL
					seen <- &copyURL
					_, _ = io.WriteString(w, testProfile)
				})
				client.server.URL += basePath
				query := url.Values{"username": {username}, "serverId": {serverID}}
				if ip != "" {
					query.Set("ip", ip)
				}
				got := callHasJoined("/hasJoined?" + query.Encode())
				if got.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body = %q", got.Code, got.Body.String())
				}
				var upstreamURL *url.URL
				select {
				case upstreamURL = <-seen:
				default:
					t.Fatal("upstream was not called")
				}
				wantPath := strings.TrimSuffix(basePath, "/") + "/session/minecraft/hasJoined"
				if upstreamURL.Path != wantPath {
					t.Errorf("upstream path = %q, want %q", upstreamURL.Path, wantPath)
				}
				if !reflect.DeepEqual(upstreamURL.Query(), query) {
					t.Errorf("upstream query = %v, want %v", upstreamURL.Query(), query)
				}
				assertCachedURL(t, username, client.server.URL)
			})
		}
	}
}

func TestHasJoinedMissingParameters(t *testing.T) {
	for _, query := range []string{"", "username=Player", "serverId=hash", "username=&serverId=hash", "username=Player&serverId="} {
		t.Run(query, func(t *testing.T) {
			resetHandlerState(t)
			var calls testCallLog
			addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("unexpected")
				w.WriteHeader(http.StatusNoContent)
			})
			playerCache.Store("Player", "unchanged")
			got := callHasJoined("/hasJoined?" + query)
			if got.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", got.Code)
			}
			calls.assert(t)
			assertCachedURL(t, "Player", "unchanged")
		})
	}
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return roundTrip(r)
}

type testTimeoutError struct{}

func (testTimeoutError) Error() string   { return "upstream timed out" }
func (testTimeoutError) Timeout() bool   { return true }
func (testTimeoutError) Temporary() bool { return true }

type testReadFailure struct{}

func (testReadFailure) Read(p []byte) (int, error) {
	return copy(p, testProfile), errors.New("upstream body read failed")
}
func (testReadFailure) Close() error { return nil }

func TestHasJoinedClientFailures(t *testing.T) {
	tests := []struct {
		name      string
		roundTrip testRoundTripper
	}{
		{"transport", func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("upstream connection failed")
		}},
		{"timeout", func(r *http.Request) (*http.Response, error) {
			return nil, testTimeoutError{}
		}},
		{"body read", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: testReadFailure{}}, nil
		}},
	}
	for _, test := range tests {
		for _, success := range []bool{false, true} {
			name := test.name + "/no match"
			if success {
				name = test.name + "/success"
			}
			t.Run(name, func(t *testing.T) {
				resetHandlerState(t)
				failed := &authClient{server: AuthServer{URL: "http://failure.invalid"}, client: &http.Client{Transport: test.roundTrip}}
				authClients = append(authClients, failed)
				fallback := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					if success {
						_, _ = io.WriteString(w, testProfile)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				})
				playerCache.Store("Player", failed.server.URL)
				got := callHasJoined("/hasJoined?username=Player&serverId=hash")
				wantStatus, wantURL := http.StatusBadGateway, failed.server.URL
				if success {
					wantStatus, wantURL = http.StatusOK, fallback.server.URL
				}
				if got.Code != wantStatus {
					t.Fatalf("status = %d, want %d", got.Code, wantStatus)
				}
				assertCachedURL(t, "Player", wantURL)
			})
		}
	}
}

func TestHasJoinedCancellation(t *testing.T) {
	for _, when := range []string{"before request", "during no match", "during error", "during success"} {
		t.Run(when, func(t *testing.T) {
			resetHandlerState(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls testCallLog
			first := &authClient{server: AuthServer{URL: "http://first.invalid"}, client: &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.add("first")
				if r.Context().Value(testContextKey{}) != "incoming" {
					t.Error("upstream request did not inherit incoming context")
				}
				cancel()
				if when == "during error" {
					return nil, context.Canceled
				}
				status, body := http.StatusNoContent, ""
				if when == "during success" {
					status, body = http.StatusOK, testProfile
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			authClients = append(authClients, first)
			addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("unexpected fallback")
				_, _ = io.WriteString(w, testProfile)
			})
			playerCache.Store("Player", "http://previous.invalid")
			if when == "before request" {
				cancel()
			}
			ctx = context.WithValue(ctx, testContextKey{}, "incoming")
			request := httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=hash", nil).WithContext(ctx)
			hasJoinedHandler(httptest.NewRecorder(), request)
			if when == "before request" {
				calls.assert(t)
			} else {
				calls.assert(t, "first")
			}
			assertCachedURL(t, "Player", "http://previous.invalid")
		})
	}
}

type testContextKey struct{}

func TestHasJoinedProfileSizeLimit(t *testing.T) {
	for _, size := range []int{maxProfileResponseSize - 1, maxProfileResponseSize, maxProfileResponseSize + 1} {
		name := "within limit"
		if size == maxProfileResponseSize {
			name = "at limit"
		} else if size > maxProfileResponseSize {
			name = "over limit"
		}
		t.Run(name, func(t *testing.T) {
			resetHandlerState(t)
			body := testProfile + strings.Repeat(" ", size-len(testProfile))
			upstream := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			got := callHasJoined("/hasJoined?username=Player&serverId=hash")
			if size > maxProfileResponseSize {
				if got.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502", got.Code)
				}
				if _, ok := playerCache.Load("Player"); ok {
					t.Fatal("oversized profile populated cache")
				}
				return
			}
			if got.Code != http.StatusOK || got.Body.String() != body {
				t.Fatalf("response = %d with %d bytes, want 200 with %d original bytes", got.Code, got.Body.Len(), size)
			}
			assertCachedURL(t, "Player", upstream.server.URL)
		})
	}
}

func TestHasJoinedInvalidUpstreamURL(t *testing.T) {
	for _, upstreamURL := range []string{"://invalid", "https://", "ftp://example.com", "/relative"} {
		t.Run(upstreamURL, func(t *testing.T) {
			resetHandlerState(t)
			authClients = []*authClient{{server: AuthServer{URL: upstreamURL}, client: &http.Client{}}}
			playerCache.Store("Player", upstreamURL)
			got := callHasJoined("/hasJoined?username=Player&serverId=hash")
			if got.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", got.Code)
			}
			assertCachedURL(t, "Player", upstreamURL)
		})
	}
}
