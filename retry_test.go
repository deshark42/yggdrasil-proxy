package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRetryableNetworkErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"wrapped EOF", fmt.Errorf("transport: %w", io.EOF), true},
		{"wrapped unexpected EOF", &url.Error{Op: "Get", URL: "https://example.invalid", Err: io.ErrUnexpectedEOF}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"timeout", testTimeoutError{}, true},
		{"connection refused", syscall.ECONNREFUSED, true},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"temporary DNS", &net.DNSError{Err: "temporary failure", Name: "upstream.invalid", IsTemporary: true}, true},
		{"DNS not found", &net.DNSError{Err: "no such host", Name: "upstream.invalid", IsNotFound: true}, false},
		{"canceled", context.Canceled, false},
		{"wrapped canceled", &url.Error{Op: "Get", URL: "https://example.invalid", Err: context.Canceled}, false},
		{"TLS record", tls.RecordHeaderError{Msg: "not a TLS record"}, false},
		{"certificate", x509.UnknownAuthorityError{}, false},
		{"invalid address", &net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Err: "invalid port", Addr: "upstream.invalid:bad"}}, false},
		{"invalid address type", &net.OpError{Op: "dial", Net: "tcp", Err: net.InvalidAddrError("bad address")}, false},
		{"generic", errors.New("unrelated application error"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableNetworkError(test.err); got != test.want {
				t.Errorf("retryableNetworkError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func retryTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestAuthenticationRetryRecoversBeforeFallback(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			resetHandlerState(t)
			var output bytes.Buffer
			logger, err := newLogger(&output, "debug", format)
			if err != nil {
				t.Fatal(err)
			}
			previous := slog.Default()
			slog.SetDefault(logger)
			t.Cleanup(func() { slog.SetDefault(previous) })
			calls, fallbackCalls := 0, 0
			client := newAuthClient(AuthServer{Name: "recovering", URL: "https://recovering.invalid", Timeout: 5})
			client.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, &url.Error{Op: "Get", URL: "https://user-secret:password-secret@recovering.invalid/private-secret?serverId=server-secret", Err: syscall.ECONNREFUSED}
				}
				return retryTestResponse(http.StatusOK, testProfile), nil
			})
			fallback := newAuthClient(AuthServer{Name: "fallback", URL: "https://fallback.invalid", Timeout: 5})
			fallback.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				fallbackCalls++
				return retryTestResponse(http.StatusOK, testProfile), nil
			})
			authClients = []*authClient{client, fallback}
			response := callHasJoined("/hasJoined?username=Player&serverId=server-secret&ip=ip-secret")
			if response.Code != 200 || response.Body.String() != testProfile || calls != 2 || fallbackCalls != 0 {
				t.Fatalf("response = %d %q, calls = %d, fallback = %d", response.Code, response.Body.String(), calls, fallbackCalls)
			}
			assertCachedURL(t, "Player", client.server.URL)
			if format == "json" {
				records := decodeAuthenticationLogs(t, &output)
				assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
				summary := assertAuthenticationSummary(t, records, "success", "INFO", 200, 2)
				assertLoggingFields(t, summary, loggingRecord{"had_upstream_error": true, "upstream": "recovering"})
				attempts := authenticationEvents(records, "upstream_attempt")
				if len(attempts) != 2 {
					t.Fatalf("attempt logs = %d, want 2", len(attempts))
				}
				assertLoggingFields(t, attempts[0], loggingRecord{"attempt": float64(1), "outcome": "upstream_error"})
				assertLoggingFields(t, attempts[1], loggingRecord{"attempt": float64(2), "outcome": "success", "retry": float64(1), "max_retries": float64(2)})
				for _, key := range []string{"retry", "max_retries"} {
					if _, exists := attempts[0][key]; exists {
						t.Errorf("initial attempt contains %s: %#v", key, attempts[0])
					}
				}
			} else {
				assertConsolePhrases(t, output.String(), "Retry 1 of 2 for this upstream.", "Player Player authenticated via recovering", "2 attempts", "Request "+response.Header().Get("X-Request-ID")+".")
				assertHumanConsoleMessage(t, output.String())
			}
			for _, secret := range []string{"user-secret", "password-secret", "private-secret", "server-secret", "ip-secret", "opaque+/="} {
				if strings.Contains(output.String(), secret) {
					t.Errorf("retry log leaked %q: %s", secret, output.String())
				}
			}
		})
	}
}

func TestAuthenticationRetryExhaustionFallsBackAndDeduplicatesCachedURL(t *testing.T) {
	resetHandlerState(t)
	output := captureAuthenticationLogs(t, "debug")
	var calls testCallLog
	var starts []time.Time
	failed := newAuthClient(AuthServer{Name: "cached", URL: "https://cached.invalid", Timeout: 5})
	failed.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.add("cached")
		starts = append(starts, time.Now())
		return nil, syscall.ECONNREFUSED
	})
	fallback := newAuthClient(AuthServer{Name: "fallback", URL: "https://fallback.invalid", Timeout: 5})
	fallback.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.add("fallback")
		return retryTestResponse(http.StatusOK, testProfile), nil
	})
	duplicate := &authClient{server: failed.server, client: failed.client}
	authClients = []*authClient{failed, duplicate, fallback, failed}
	playerCache.Store("Player", failed.server.URL)
	response := callHasJoined("/hasJoined?username=Player&serverId=secret")
	if response.Code != 200 || response.Body.String() != testProfile {
		t.Fatalf("fallback response = %d %q", response.Code, response.Body.String())
	}
	calls.assert(t, "cached", "cached", "cached", "fallback")
	assertCachedURL(t, "Player", fallback.server.URL)
	if len(starts) != 3 {
		t.Fatalf("unreachable upstream calls = %d, want 3 total requests (initial plus 2 retries)", len(starts))
	}
	for i, delay := range []time.Duration{200 * time.Millisecond, 400 * time.Millisecond} {
		if elapsed := starts[i+1].Sub(starts[i]); elapsed < delay-20*time.Millisecond {
			t.Errorf("retry %d started after %v, want at least approximately %v", i+1, elapsed, delay)
		}
	}
	records := decodeAuthenticationLogs(t, output)
	assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
	summary := assertAuthenticationSummary(t, records, "success", "INFO", 200, 4)
	assertLoggingFields(t, summary, loggingRecord{"cache_hit": true, "had_upstream_error": true, "upstream": "fallback"})
	attempts := authenticationEvents(records, "upstream_attempt")
	if len(attempts) != 4 {
		t.Fatalf("attempt logs = %d, want 4", len(attempts))
	}
	for i, record := range attempts {
		assertLoggingFields(t, record, loggingRecord{"attempt": float64(i + 1)})
		if i > 0 && i < 3 {
			assertLoggingFields(t, record, loggingRecord{"retry": float64(i), "max_retries": float64(2), "upstream": "cached"})
		} else {
			for _, key := range []string{"retry", "max_retries"} {
				if _, exists := record[key]; exists {
					t.Errorf("initial upstream attempt contains %s: %#v", key, record)
				}
			}
		}
	}
}

type retryClosingBody struct {
	io.Reader
	closed *int
}

func (body *retryClosingBody) Close() error {
	(*body.closed)++
	return nil
}

type retryUnexpectedEOFReader struct{}

func (retryUnexpectedEOFReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestAuthenticationRetryClosesFailedAndSuccessfulResponseBodies(t *testing.T) {
	resetHandlerState(t)
	captureAuthenticationLogs(t, "debug")
	calls, closed := 0, 0
	client := newAuthClient(AuthServer{Name: "body", URL: "https://body.invalid", Timeout: 5})
	client.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if closed != calls-1 {
			t.Errorf("attempt %d began with %d closed bodies", calls, closed)
		}
		var reader io.Reader = strings.NewReader(testProfile)
		if calls == 1 {
			reader = retryUnexpectedEOFReader{}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &retryClosingBody{Reader: reader, closed: &closed}}, nil
	})
	authClients = []*authClient{client}
	response := callHasJoined("/hasJoined?username=Player&serverId=secret")
	if response.Code != 200 || calls != 2 || closed != 2 {
		t.Errorf("response status = %d, calls = %d, closed bodies = %d; want 200, 2, 2", response.Code, calls, closed)
	}
	assertCachedURL(t, "Player", client.server.URL)
}

func TestAuthenticationDoesNotRetryPermanentFailuresOrResponses(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		status     int
		body       string
		wantStatus int
	}{
		{"HTTP 503", nil, 503, "body-secret", 502},
		{"HTTP 429", nil, 429, "body-secret", 502},
		{"no match", nil, 204, "", 204},
		{"invalid JSON", nil, 200, "invalid profile", 502},
		{"invalid profile", nil, 200, "{}", 502},
		{"invalid UUID", nil, 200, `{"id":"` + strings.Repeat("z", 32) + `","name":"Player"}`, 502},
		{"permanent DNS", &net.DNSError{Err: "no such host", Name: "missing.invalid", IsNotFound: true}, 0, "", 502},
		{"TLS", tls.RecordHeaderError{Msg: "TLS failed"}, 0, "", 502},
		{"canceled error without downstream cancellation", context.Canceled, 0, "", 502},
		{"unrelated error", errors.New("unrelated application error"), 0, "", 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetHandlerState(t)
			output := captureAuthenticationLogs(t, "debug")
			calls := 0
			client := newAuthClient(AuthServer{Name: "once", URL: "https://once.invalid", Timeout: 5})
			client.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if test.err != nil {
					return nil, test.err
				}
				return retryTestResponse(test.status, test.body), nil
			})
			authClients = []*authClient{client}
			response := callHasJoined("/hasJoined?username=Player&serverId=secret")
			if response.Code != test.wantStatus || calls != 1 {
				t.Errorf("status = %d, calls = %d; want %d, 1", response.Code, calls, test.wantStatus)
			}
			if _, ok := playerCache.Load("Player"); ok {
				t.Error("unsuccessful authentication populated cache")
			}
			attempts := authenticationEvents(decodeAuthenticationLogs(t, output), "upstream_attempt")
			if len(attempts) != 1 {
				t.Errorf("attempt logs = %d, want 1", len(attempts))
			}
		})
	}
}

func TestAuthenticationRetryWaitHonorsCancellationAndTotalBudget(t *testing.T) {
	for _, stop := range []string{"client cancel", "total budget"} {
		t.Run(stop, func(t *testing.T) {
			resetHandlerState(t)
			output := captureAuthenticationLogs(t, "debug")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, fallbackCalls := 0, 0
			client := newAuthClient(AuthServer{Name: "unreachable", URL: "https://unreachable.invalid", Timeout: 5})
			client.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if stop == "client cancel" {
					timer := time.AfterFunc(20*time.Millisecond, cancel)
					t.Cleanup(func() { timer.Stop() })
				}
				return nil, syscall.ECONNREFUSED
			})
			fallback := newAuthClient(AuthServer{Name: "fallback", URL: "https://fallback.invalid", Timeout: 5})
			fallback.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				fallbackCalls++
				return retryTestResponse(http.StatusOK, testProfile), nil
			})
			authClients = []*authClient{client, fallback}
			playerCache.Store("Player", client.server.URL)
			budget := time.Second
			if stop == "total budget" {
				budget = 30 * time.Millisecond
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=secret", nil).WithContext(ctx)
			started := time.Now()
			hasJoinedHandler(budget)(response, request)
			if elapsed := time.Since(started); elapsed >= 180*time.Millisecond {
				t.Errorf("retry wait ignored cancellation until %v (first delay is 200ms)", elapsed)
			}
			if calls != 1 || fallbackCalls != 0 {
				t.Errorf("requests after interruption: upstream = %d, fallback = %d; want 1, 0", calls, fallbackCalls)
			}
			assertCachedURL(t, "Player", client.server.URL)
			records := decodeAuthenticationLogs(t, output)
			assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
			outcome, level, status := "canceled", "INFO", 0
			if stop == "total budget" {
				outcome, level, status = "timeout", "WARN", 502
				if response.Code != 502 {
					t.Errorf("budget status = %d, want 502", response.Code)
				}
			} else if response.Body.Len() != 0 || response.Code != http.StatusOK {
				t.Errorf("canceled request wrote a response: %d %q", response.Code, response.Body.String())
			}
			summary := assertAuthenticationSummary(t, records, outcome, level, status, 1)
			assertLoggingFields(t, summary, loggingRecord{"had_upstream_error": true})
		})
	}
}
