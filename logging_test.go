package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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

type loggingRecord map[string]any

func captureAuthenticationLogs(t *testing.T, level string) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	logger, err := newLogger(&output, level, "json")
	if err != nil {
		t.Fatal(err)
	}
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func decodeAuthenticationLogs(t *testing.T, output *bytes.Buffer) []loggingRecord {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	var records []loggingRecord
	for {
		var record loggingRecord
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode log: %v; logs = %q", err, output.String())
		}
		records = append(records, record)
	}
	return records
}

func authenticationEvents(records []loggingRecord, message string) []loggingRecord {
	var matches []loggingRecord
	for _, record := range records {
		if record["msg"] == message {
			matches = append(matches, record)
		}
	}
	return matches
}

func assertLoggingFields(t *testing.T, record loggingRecord, want loggingRecord) {
	t.Helper()
	for key, value := range want {
		if record[key] != value {
			t.Errorf("log[%s] = %#v, want %#v; record = %#v", key, record[key], value, record)
		}
	}
}

func assertAuthenticationCorrelation(t *testing.T, records []loggingRecord, requestID, username string) {
	t.Helper()
	if requestID == "" {
		t.Fatal("response has no X-Request-ID")
	}
	for _, record := range records {
		if record["msg"] != "authentication_completed" && record["msg"] != "upstream_attempt" {
			continue
		}
		assertLoggingFields(t, record, loggingRecord{"request_id": requestID, "username": username})
		duration, ok := record["duration_ms"].(float64)
		if !ok || duration < 0 {
			t.Errorf("duration_ms = %#v, want nonnegative number", record["duration_ms"])
		}
		for _, key := range []string{"serverId", "server_id", "ip", "url", "error", "body"} {
			if _, present := record[key]; present {
				t.Errorf("authentication log contains sensitive field %q", key)
			}
		}
	}
}

func assertAuthenticationSummary(t *testing.T, records []loggingRecord, outcome, level string, status, attempts int) loggingRecord {
	t.Helper()
	summaries := authenticationEvents(records, "authentication_completed")
	if len(summaries) != 1 {
		t.Fatalf("authentication summaries = %d, want exactly 1; records = %#v", len(summaries), records)
	}
	summary := summaries[0]
	assertLoggingFields(t, summary, loggingRecord{
		"outcome": outcome, "level": level, "status": float64(status), "attempts": float64(attempts),
	})
	for _, key := range []string{"cache_hit", "had_upstream_error"} {
		if _, ok := summary[key].(bool); !ok {
			t.Errorf("summary[%s] = %#v, want bool", key, summary[key])
		}
	}
	return summary
}

func upstreamLoggingHost(t *testing.T, client *authClient) string {
	t.Helper()
	parsed, err := url.Parse(client.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

func TestNewLoggerConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, level, format, wantLevel string
		wantJSON                       bool
	}{
		{"defaults", "", "", "INFO", false},
		{"debug JSON", "debug", "json", "DEBUG", true},
		{"normalized debug", "  DeBuG\t", "json", "DEBUG", true},
		{"normalized info", " INFO ", "text", "INFO", false},
		{"normalized warn", " WaRn ", "json", "WARN", true},
		{"normalized error", " ErRoR ", "text", "ERROR", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := newLogger(&output, test.level, test.format)
			if err != nil {
				t.Fatal(err)
			}
			logger.Debug("debug event")
			logger.Info("info event")
			logger.Warn("warn event")
			logger.Error("error event")
			levels := []string{"DEBUG", "INFO", "WARN", "ERROR"}
			messages := []string{"debug event", "info event", "warn event", "error event"}
			minimum := 0
			for levels[minimum] != test.wantLevel {
				minimum++
			}
			if test.wantJSON {
				records := decodeAuthenticationLogs(t, &output)
				if len(records) != len(levels)-minimum {
					t.Fatalf("records = %d, want %d", len(records), len(levels)-minimum)
				}
				for i, record := range records {
					assertLoggingFields(t, record, loggingRecord{"level": levels[minimum+i], "msg": messages[minimum+i]})
				}
			} else {
				lines := strings.Split(strings.TrimSpace(output.String()), "\n")
				if len(lines) != len(levels)-minimum {
					t.Fatalf("text records = %d, want %d: %q", len(lines), len(levels)-minimum, output.String())
				}
				for i, line := range lines {
					if !strings.Contains(line, " "+levels[minimum+i]+" ") || !strings.Contains(line, messages[minimum+i]) || strings.Contains(line, "level=") || strings.Contains(line, "msg=") || strings.Contains(line, "time=") {
						t.Errorf("unexpected text log %q", line)
					}
				}
			}
		})
	}
	for _, test := range []struct{ level, format string }{
		{"trace", "text"}, {"warning", "json"}, {"info", "xml"}, {"info", "json-secret"},
	} {
		t.Run("reject "+test.level+"/"+test.format, func(t *testing.T) {
			if _, err := newLogger(io.Discard, test.level, test.format); err == nil {
				t.Fatal("invalid logger configuration was accepted")
			}
		})
	}
}

func TestAuthenticationLogsSuccessAndCache(t *testing.T) {
	resetHandlerState(t)
	output := captureAuthenticationLogs(t, "debug")
	winner := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, testProfile)
	})
	winner.server.Name = "primary"
	previousID := ""
	for _, cached := range []bool{false, true} {
		output.Reset()
		response := callHasJoined("/hasJoined?username=Player&serverId=server-secret&ip=ip-secret")
		if response.Code != http.StatusOK || response.Body.String() != testProfile {
			t.Fatalf("response = %d %q", response.Code, response.Body.String())
		}
		records := decodeAuthenticationLogs(t, output)
		requestID := response.Header().Get("X-Request-ID")
		assertAuthenticationCorrelation(t, records, requestID, "Player")
		if requestID == previousID {
			t.Fatal("separate requests reused request_id")
		}
		previousID = requestID
		summary := assertAuthenticationSummary(t, records, "success", "INFO", 200, 1)
		assertLoggingFields(t, summary, loggingRecord{
			"cache_hit": cached, "had_upstream_error": false,
			"upstream": "primary", "upstream_host": upstreamLoggingHost(t, winner),
		})
		attempts := authenticationEvents(records, "upstream_attempt")
		if len(attempts) != 1 {
			t.Fatalf("attempt logs = %d, want 1", len(attempts))
		}
		assertLoggingFields(t, attempts[0], loggingRecord{
			"attempt": float64(1), "upstream": "primary", "upstream_host": upstreamLoggingHost(t, winner),
			"cached": cached, "status": float64(200), "outcome": "success", "level": "DEBUG",
		})
		for _, secret := range []string{"server-secret", "ip-secret", "opaque+/=", "signed+/="} {
			if strings.Contains(output.String(), secret) {
				t.Errorf("logs leaked %q", secret)
			}
		}
	}
}

func TestAuthenticationLogsFallbackCacheAndDeduplication(t *testing.T) {
	for _, cachedStatus := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(cachedStatus), func(t *testing.T) {
			resetHandlerState(t)
			output := captureAuthenticationLogs(t, "debug")
			var calls testCallLog
			priority := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("priority")
				w.WriteHeader(http.StatusNoContent)
			})
			cached := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("cached")
				w.WriteHeader(cachedStatus)
			})
			winner := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.add("winner")
				_, _ = io.WriteString(w, testProfile)
			})
			priority.server.Name, cached.server.Name, winner.server.Name = "priority", "cached", "winner"
			authClients = []*authClient{priority, cached, cached, priority, winner, winner}
			playerCache.Store("Player", cached.server.URL)
			response := callHasJoined("/hasJoined?username=Player&serverId=hash")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			calls.assert(t, "cached", "priority", "winner")
			records := decodeAuthenticationLogs(t, output)
			assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
			summary := assertAuthenticationSummary(t, records, "success", "INFO", 200, 3)
			assertLoggingFields(t, summary, loggingRecord{
				"cache_hit": true, "had_upstream_error": cachedStatus != http.StatusNoContent,
				"upstream": "winner", "upstream_host": upstreamLoggingHost(t, winner),
			})
			attempts := authenticationEvents(records, "upstream_attempt")
			if len(attempts) != 3 {
				t.Fatalf("attempt logs = %d, want 3 actual attempts", len(attempts))
			}
			for i, client := range []*authClient{cached, priority, winner} {
				status, outcome, level := http.StatusNoContent, "no_match", "DEBUG"
				if i == 0 {
					status = cachedStatus
					if cachedStatus != http.StatusNoContent {
						outcome, level = "upstream_error", "WARN"
						assertLoggingFields(t, attempts[i], loggingRecord{"reason": "unexpected_status"})
					}
				} else if i == 2 {
					status, outcome = http.StatusOK, "success"
				}
				assertLoggingFields(t, attempts[i], loggingRecord{
					"attempt": float64(i + 1), "upstream": client.server.Name, "upstream_host": upstreamLoggingHost(t, client),
					"cached": i == 0, "status": float64(status), "outcome": outcome, "level": level,
				})
			}
		})
	}
}

func TestAuthenticationLogsNoMatchAndUpstreamError(t *testing.T) {
	for _, test := range []struct {
		name, outcome, level string
		statuses             []int
		status               int
		hadError             bool
	}{
		{"no match", "no_match", "INFO", []int{204, 204}, 204, false},
		{"mixed failures", "upstream_error", "WARN", []int{503, 204}, 502, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetHandlerState(t)
			output := captureAuthenticationLogs(t, "debug")
			for _, status := range test.statuses {
				addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			}
			response := callHasJoined("/hasJoined?username=Player&serverId=hash")
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			records := decodeAuthenticationLogs(t, output)
			assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
			summary := assertAuthenticationSummary(t, records, test.outcome, test.level, test.status, len(test.statuses))
			assertLoggingFields(t, summary, loggingRecord{"cache_hit": false, "had_upstream_error": test.hadError})
			if attempts := authenticationEvents(records, "upstream_attempt"); len(attempts) != len(test.statuses) {
				t.Errorf("attempt logs = %d, want %d", len(attempts), len(test.statuses))
			}
		})
	}
}

func TestAuthenticationLogsInvalidRequest(t *testing.T) {
	for _, query := range []string{"", "username=Player", "serverId=secret", "username=&serverId=secret", "username=Player&serverId="} {
		t.Run(query, func(t *testing.T) {
			resetHandlerState(t)
			output := captureAuthenticationLogs(t, "debug")
			response := callHasJoined("/hasJoined?" + query)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.Code)
			}
			records := decodeAuthenticationLogs(t, output)
			username := ""
			if strings.Contains(query, "username=Player") {
				username = "Player"
			}
			assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), username)
			summary := assertAuthenticationSummary(t, records, "invalid_request", "WARN", 400, 0)
			assertLoggingFields(t, summary, loggingRecord{"cache_hit": false, "had_upstream_error": false})
			if attempts := authenticationEvents(records, "upstream_attempt"); len(attempts) != 0 {
				t.Errorf("invalid request produced %d attempt logs", len(attempts))
			}
		})
	}
}

func TestAuthenticationLogsTimeoutAndCancellation(t *testing.T) {
	for _, when := range []string{"total timeout", "canceled before attempt", "canceled during attempt"} {
		t.Run(when, func(t *testing.T) {
			resetHandlerState(t)
			output := captureAuthenticationLogs(t, "debug")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := newAuthClient(AuthServer{Name: "blocked", URL: "http://blocked.invalid", Timeout: 5})
			client.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				if when == "canceled during attempt" {
					cancel()
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			authClients = []*authClient{client}
			if when == "canceled before attempt" {
				cancel()
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=secret", nil).WithContext(ctx)
			hasJoinedHandler(20*time.Millisecond)(response, request)
			records := decodeAuthenticationLogs(t, output)
			assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
			outcome, level, status, count := "canceled", "INFO", 0, 1
			if when == "total timeout" {
				outcome, level, status = "timeout", "WARN", 502
				if response.Code != 502 {
					t.Errorf("timeout status = %d, want 502", response.Code)
				}
			} else if response.Body.Len() != 0 || response.Flushed || response.Code != http.StatusOK {
				t.Errorf("canceled request wrote a response: %#v", response)
			}
			if when == "canceled before attempt" {
				count = 0
			}
			summary := assertAuthenticationSummary(t, records, outcome, level, status, count)
			assertLoggingFields(t, summary, loggingRecord{"cache_hit": false, "had_upstream_error": false})
			attempts := authenticationEvents(records, "upstream_attempt")
			if len(attempts) != count {
				t.Fatalf("attempt logs = %d, want %d", len(attempts), count)
			}
			if count != 0 {
				reason := "upstream_canceled"
				if when == "total timeout" {
					reason = "upstream_timeout"
				}
				assertLoggingFields(t, attempts[0], loggingRecord{
					"attempt": float64(1), "status": float64(0), "outcome": "upstream_error", "level": "DEBUG", "reason": reason,
				})
			}
		})
	}
}

func TestAuthenticationLogsRedactUpstreamFailures(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		body         string
		err          error
		attempts     int
	}{
		{"JSON syntax", "invalid_profile_json", "!json-body-secret", nil, 1},
		{"JSON type", "invalid_profile_json", `{"id":"0123456789abcdef0123456789abcdef","name":{"json-body-secret":true}}`, nil, 1},
		{"network", "request_error", "", &url.Error{Op: "Get", URL: "https://error-user:error-password@error.invalid/error-path?token=error-query#fragment-secret", Err: errors.New("raw-error-secret")}, 1},
		{"upstream timeout", "upstream_timeout", "", testTimeoutError{}, 3},
		{"DNS", "dns_error", "", &net.DNSError{Err: "raw-error-secret", Name: "dns-secret.invalid"}, 3},
		{"connection", "connection_error", "", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, 3},
		{"TLS", "tls_error", "", tls.RecordHeaderError{Msg: "tls-error-secret"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetHandlerState(t)
			output := captureAuthenticationLogs(t, "debug")
			client := newAuthClient(AuthServer{
				Name: "safe-name", URL: "https://credential-user:credential-password@safe.invalid:8443/private-path?token=config-query-secret#fragment-secret", Timeout: 5,
			})
			client.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})
			authClients = []*authClient{client}
			response := callHasJoined("/hasJoined?username=Player&serverId=server-id-secret&ip=ip-secret")
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", response.Code)
			}
			records := decodeAuthenticationLogs(t, output)
			assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
			summary := assertAuthenticationSummary(t, records, "upstream_error", "WARN", 502, test.attempts)
			assertLoggingFields(t, summary, loggingRecord{"had_upstream_error": true})
			attempts := authenticationEvents(records, "upstream_attempt")
			if len(attempts) != test.attempts {
				t.Fatalf("attempt logs = %d, want %d", len(attempts), test.attempts)
			}
			status := 0
			if test.err == nil {
				status = 200
			}
			for i, attempt := range attempts {
				assertLoggingFields(t, attempt, loggingRecord{
					"attempt": float64(i + 1), "upstream": "safe-name", "upstream_host": "safe.invalid:8443",
					"cached": false, "status": float64(status), "outcome": "upstream_error", "level": "WARN", "reason": test.reason,
				})
				if i > 0 {
					assertLoggingFields(t, attempt, loggingRecord{"retry": float64(i), "max_retries": float64(2)})
				}
			}
			for _, secret := range []string{
				"credential-user", "credential-password", "private-path", "config-query-secret", "fragment-secret",
				"server-id-secret", "ip-secret", "error-user", "error-password", "error-path", "error-query",
				"raw-error-secret", "dns-secret", "tls-error-secret", "json-body-secret", "'!'",
			} {
				if strings.Contains(output.String(), secret) {
					t.Errorf("logs leaked %q: %s", secret, output.String())
				}
			}
		})
	}
}

func TestAuthenticationLogsRequestIDsAndUsernameBound(t *testing.T) {
	resetHandlerState(t)
	output := captureAuthenticationLogs(t, "debug")
	username := strings.Repeat("x", 128) + "username-tail-secret"
	seen := make(map[string]bool)
	for i := 0; i < 12; i++ {
		output.Reset()
		request := httptest.NewRequest(http.MethodGet, "/hasJoined?username="+url.QueryEscape(username)+"&serverId=secret", nil)
		request.Header.Set("X-Request-ID", "attacker-request-id")
		response := httptest.NewRecorder()
		hasJoinedHandler(time.Second)(response, request)
		requestID := response.Header().Get("X-Request-ID")
		if requestID == "attacker-request-id" || seen[requestID] {
			t.Fatalf("request_id is trusted or reused: %q", requestID)
		}
		seen[requestID] = true
		records := decodeAuthenticationLogs(t, output)
		assertAuthenticationCorrelation(t, records, requestID, strings.Repeat("x", 128)+"...")
		assertAuthenticationSummary(t, records, "no_match", "INFO", 204, 0)
		if strings.Contains(output.String(), "username-tail-secret") || strings.Contains(output.String(), "attacker-request-id") {
			t.Fatalf("logs contain unbounded username or supplied ID: %s", output.String())
		}
	}
}

func TestAuthenticationLogsDefaultInfoOmitsDebugAttempts(t *testing.T) {
	resetHandlerState(t)
	output := captureAuthenticationLogs(t, "")
	addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, testProfile) })
	response := callHasJoined("/hasJoined?username=Player&serverId=secret")
	records := decodeAuthenticationLogs(t, output)
	assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
	assertAuthenticationSummary(t, records, "success", "INFO", 200, 1)
	if attempts := authenticationEvents(records, "upstream_attempt"); len(attempts) != 0 {
		t.Errorf("default INFO emitted %d DEBUG attempt records", len(attempts))
	}
}

type loggingFailedResponseWriter struct {
	header http.Header
	status int
}

func (w *loggingFailedResponseWriter) Header() http.Header    { return w.header }
func (w *loggingFailedResponseWriter) WriteHeader(status int) { w.status = status }
func (w *loggingFailedResponseWriter) Write([]byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return 0, errors.New("response-write-secret")
}

func TestAuthenticationLogsResponseWriteFailure(t *testing.T) {
	resetHandlerState(t)
	output := captureAuthenticationLogs(t, "debug")
	addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, testProfile) })
	response := &loggingFailedResponseWriter{header: make(http.Header)}
	request := httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=secret", nil)
	hasJoinedHandler(time.Second)(response, request)
	records := decodeAuthenticationLogs(t, output)
	assertAuthenticationCorrelation(t, records, response.Header().Get("X-Request-ID"), "Player")
	assertAuthenticationSummary(t, records, "response_write_error", "WARN", 200, 1)
	if strings.Contains(output.String(), "response-write-secret") {
		t.Fatal("logs leaked raw response write error")
	}
}
