package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func assertHumanConsoleMessage(t *testing.T, message string) {
	t.Helper()
	for _, machine := range []string{
		"authentication_completed", "upstream_attempt", "upstream_configured", "proxy_starting",
		"default_config_created", "logging_configuration_failed", "configuration_failed", "server_failed",
		"request_id", "duration_ms", "cache_hit", "had_upstream_error", "upstream_host",
		"timeout_seconds", "auth_timeout_seconds", "cache_max_entries", "cache_ttl_seconds", "cache_cleanup_interval_seconds",
		"config_path", "outcome=", "status=", "attempts=", "username=", "reason=", " | ",
	} {
		if strings.Contains(message, machine) {
			t.Errorf("human-readable message contains machine notation %q: %q", machine, message)
		}
	}
	if strings.Contains(message, "=") {
		t.Errorf("human-readable message contains key=value notation: %q", message)
	}
}

func assertConsolePhrases(t *testing.T, message string, phrases ...string) {
	t.Helper()
	for _, phrase := range phrases {
		if !strings.Contains(message, phrase) {
			t.Errorf("message %q does not preserve %q", message, phrase)
		}
	}
}

func TestConsoleApplicationMessages(t *testing.T) {
	for _, test := range []struct {
		event   string
		attrs   []slog.Attr
		phrases []string
	}{
		{
			"upstream_configured",
			[]slog.Attr{slog.String("upstream", "Mojang"), slog.String("upstream_host", "sessionserver.mojang.com"), slog.Int("priority", 2), slog.Int("timeout_seconds", 5)},
			[]string{"Configured upstream", "Mojang", "sessionserver.mojang.com", "priority 2", "5 s timeout"},
		},
		{
			"proxy_starting",
			[]slog.Attr{slog.String("address", ":32217"), slog.Int("upstreams", 3), slog.Int("auth_timeout_seconds", 15), slog.Int("cache_max_entries", 4096), slog.Int("cache_ttl_seconds", 3600), slog.Int("cache_cleanup_interval_seconds", 60)},
			[]string{"Starting Yggdrasil Proxy", ":32217", "3 upstreams", "15 s authentication limit", "4096 players", "3600 s", "every 60 s"},
		},
		{
			"default_config_created",
			[]slog.Attr{slog.String("config_path", "config.json")},
			[]string{"Created default configuration", "config.json"},
		},
		{
			"logging_configuration_failed",
			[]slog.Attr{slog.String("error", "unsupported log level")},
			[]string{"Cannot configure logging", "unsupported log level"},
		},
		{
			"configuration_failed",
			[]slog.Attr{slog.String("config_path", "config.json"), slog.String("error", "permission denied")},
			[]string{"Cannot load configuration", "config.json", "permission denied"},
		},
		{
			"server_failed",
			[]slog.Attr{slog.String("address", ":32217"), slog.String("error", "address already in use")},
			[]string{"HTTP server", ":32217", "failed", "address already in use"},
		},
	} {
		t.Run(test.event, func(t *testing.T) {
			message := consoleSentence(test.event, test.attrs)
			assertHumanConsoleMessage(t, message)
			assertConsolePhrases(t, message, test.phrases...)
		})
	}
}

func consoleSummaryTestAttrs(outcome string, attempts int, cache, hadError bool) []slog.Attr {
	status := http.StatusOK
	switch outcome {
	case "no_match":
		status = http.StatusNoContent
	case "upstream_error", "timeout":
		status = http.StatusBadGateway
	case "invalid_request":
		status = http.StatusBadRequest
	case "canceled", "internal_error":
		status = 0
	}
	return []slog.Attr{
		slog.String("request_id", "request-42"), slog.String("username", "Steve"),
		slog.String("outcome", outcome), slog.Int("status", status), slog.Int("attempts", attempts),
		slog.Bool("cache_hit", cache), slog.Bool("had_upstream_error", hadError), slog.Float64("duration_ms", 118.6),
		slog.String("upstream", "Mojang"), slog.String("upstream_host", "sessionserver.mojang.com"),
	}
}

func TestConsoleAuthenticationOutcomes(t *testing.T) {
	for _, test := range []struct {
		outcome string
		phrases []string
	}{
		{"success", []string{"Player Steve authenticated via Mojang", "sessionserver.mojang.com"}},
		{"no_match", []string{"No upstream authenticated player Steve", "HTTP 204"}},
		{"upstream_error", []string{"Could not authenticate player Steve", "upstream failed", "HTTP 502"}},
		{"invalid_request", []string{"Rejected authentication request", "Steve", "username or server ID is missing", "HTTP 400"}},
		{"timeout", []string{"Authentication for player Steve", "exceeded the total time limit", "HTTP 502"}},
		{"canceled", []string{"Authentication for player Steve", "canceled by the client", "No response was sent"}},
		{"response_write_error", []string{"Could not deliver the response", "Steve", "after authentication via Mojang", "sessionserver.mojang.com"}},
		{"internal_error", []string{"Authentication for player Steve", "internal error"}},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			message := consoleSentence("authentication_completed", consoleSummaryTestAttrs(test.outcome, 2, false, false))
			assertHumanConsoleMessage(t, message)
			assertConsolePhrases(t, message, test.phrases...)
			assertConsolePhrases(t, message, "118.6 ms", "2 attempts", "Request request-42.")
			if strings.Contains(message, "cached") || strings.Contains(message, "earlier upstream failed") {
				t.Errorf("uncached request acquired cache or fallback details: %q", message)
			}
		})
	}
}

func TestConsoleAuthenticationRetainsCacheAndFallbackDetails(t *testing.T) {
	message := consoleSentence("authentication_completed", consoleSummaryTestAttrs("success", 2, true, true))
	assertHumanConsoleMessage(t, message)
	assertConsolePhrases(t, message, "2 attempts", "cached upstream was selected first", "earlier upstream failed", "Request request-42.")
	message = consoleSentence("authentication_completed", consoleSummaryTestAttrs("success", 1, false, false))
	want := "Player Steve authenticated via Mojang (sessionserver.mojang.com) in 118.6 ms after 1 attempt. Request request-42."
	if message != want {
		t.Errorf("single-attempt success = %q, want %q", message, want)
	}
	if strings.Contains(message, "1 attempts") {
		t.Errorf("single attempt uses plural: %q", message)
	}
}

func TestConsoleAuthenticationPreservesEarlierFailureAcrossFinalOutcomes(t *testing.T) {
	for _, outcome := range []string{"success", "timeout", "canceled", "response_write_error", "internal_error", "upstream_error"} {
		t.Run(outcome, func(t *testing.T) {
			message := consoleSentence("authentication_completed", consoleSummaryTestAttrs(outcome, 2, false, true))
			assertHumanConsoleMessage(t, message)
			if outcome == "upstream_error" {
				if strings.Contains(message, "An earlier upstream failed.") {
					t.Errorf("upstream failure summary repeats an earlier-failure explanation: %q", message)
				}
			} else {
				assertConsolePhrases(t, message, "An earlier upstream failed.")
			}
		})
	}
}

func TestConsoleMissingUsernameDoesNotInventAPlayer(t *testing.T) {
	attrs := consoleSummaryTestAttrs("invalid_request", 0, false, false)
	for i := range attrs {
		if attrs[i].Key == "username" {
			attrs[i] = slog.String("username", "")
		}
	}
	message := consoleSentence("authentication_completed", attrs)
	assertHumanConsoleMessage(t, message)
	assertConsolePhrases(t, message, "Rejected authentication request", "username or server ID is missing", "HTTP 400", "0 attempts", "Request request-42.")
	if strings.Contains(message, "player") || strings.Contains(message, "\"\"") {
		t.Errorf("missing username was rendered as an invented player: %q", message)
	}
}

func TestConsoleUpstreamAttemptReasons(t *testing.T) {
	for _, test := range []struct {
		outcome, reason, phrase string
		status                  int
	}{
		{"success", "", "authenticated the player", 200},
		{"no_match", "", "did not authenticate the player", 204},
		{"upstream_error", "invalid_upstream_url", "could not use the configured upstream address", 0},
		{"upstream_error", "request_build_error", "could not create the upstream request", 0},
		{"upstream_error", "upstream_timeout", "timed out", 0},
		{"upstream_error", "upstream_canceled", "was canceled", 0},
		{"upstream_error", "dns_error", "failed to resolve the upstream host", 0},
		{"upstream_error", "connection_error", "encountered an upstream connection error", 0},
		{"upstream_error", "connection_error", "encountered an upstream connection error", 200},
		{"upstream_error", "tls_error", "failed TLS verification or negotiation", 0},
		{"upstream_error", "request_error", "failed after", 0},
		{"upstream_error", "unexpected_status", "received an unexpected HTTP status", 503},
		{"upstream_error", "response_read_error", "failed to read the profile response", 200},
		{"upstream_error", "profile_too_large", "received a profile larger than 1 MiB", 200},
		{"upstream_error", "invalid_profile_json", "received invalid profile JSON", 200},
		{"upstream_error", "invalid_profile", "received a profile with a missing or invalid ID or name", 200},
		{"upstream_error", "invalid_profile_uuid", "received a profile with an invalid UUID", 200},
	} {
		t.Run(test.outcome+"/"+test.reason+"/"+strconv.Itoa(test.status), func(t *testing.T) {
			attrs := []slog.Attr{
				slog.String("username", "Steve"), slog.String("request_id", "request-42"),
				slog.String("upstream", "Mojang"), slog.String("upstream_host", "sessionserver.mojang.com"),
				slog.String("outcome", test.outcome), slog.String("reason", test.reason),
				slog.Int("attempt", 3), slog.Int("status", test.status), slog.Bool("cached", true), slog.Float64("duration_ms", 12.5),
			}
			message := consoleSentence("upstream_attempt", attrs)
			assertHumanConsoleMessage(t, message)
			assertConsolePhrases(t, message, "Attempt 3", "player Steve", "Mojang", "sessionserver.mojang.com", test.phrase, "12.5 ms", "selected from the cache", "Request request-42.")
			if test.reason == "connection_error" && test.status == http.StatusOK {
				assertConsolePhrases(t, message, "HTTP 200")
				if strings.Contains(message, "failed to connect") {
					t.Errorf("body read connection error was described as a connection establishment failure: %q", message)
				}
			}
			if test.reason != "" && strings.Contains(message, test.reason) {
				t.Errorf("attempt contains machine reason %q: %q", test.reason, message)
			}
			if test.status == 0 {
				assertConsolePhrases(t, message, "no HTTP response was received")
				if strings.Contains(message, "HTTP 0") {
					t.Errorf("no response was described as HTTP 0: %q", message)
				}
			} else {
				if !strings.Contains(message, http.StatusText(test.status)) && !strings.Contains(message, "HTTP "+strconv.Itoa(test.status)) {
					t.Errorf("message lost HTTP status %d: %q", test.status, message)
				}
			}
		})
	}
}

func TestConsoleRealAuthenticationSuccessAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "success"
		if fallback {
			name = "fallback"
		}
		t.Run(name, func(t *testing.T) {
			resetHandlerState(t)
			var output bytes.Buffer
			logger, err := newLogger(&output, "debug", "text")
			if err != nil {
				t.Fatal(err)
			}
			previous := slog.Default()
			slog.SetDefault(logger)
			t.Cleanup(func() { slog.SetDefault(previous) })
			if fallback {
				failed := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, "response-body-secret")
				})
				failed.server.Name = "unavailable"
			}
			winner := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, testProfile)
			})
			winner.server.Name = "trusted"
			upstreamURL, err := url.Parse(winner.server.URL)
			if err != nil {
				t.Fatal(err)
			}
			upstreamURL.User = url.UserPassword("credential-user-secret", "credential-password-secret")
			upstreamURL.Path = "/private-path-secret"
			upstreamURL.RawQuery = "token=config-query-secret"
			upstreamURL.Fragment = "fragment-secret"
			winner.server.URL = upstreamURL.String()
			for _, cached := range []bool{false, true} {
				output.Reset()
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "/hasJoined?username=Player&serverId=server-id-secret&ip=ip-secret", nil)
				hasJoinedHandler(defaultAuthTimeout)(response, request)
				if response.Code != http.StatusOK || response.Body.String() != testProfile {
					t.Fatalf("authentication response = %d %q", response.Code, response.Body.String())
				}
				message := output.String()
				assertHumanConsoleMessage(t, message)
				assertConsolePhrases(t, message, "Player Player authenticated via trusted", upstreamURL.Host, "Request "+response.Header().Get("X-Request-ID")+".", " ms")
				if response.Header().Get("X-Request-ID") == "" {
					t.Fatal("request ID is absent")
				}
				if strings.Count(message, "Player Player authenticated via trusted") != 1 {
					t.Errorf("request did not produce exactly one readable success summary: %q", message)
				}
				if cached {
					assertConsolePhrases(t, message, "1 attempt.", "cached upstream was selected first", "selected from the cache")
					if strings.Contains(message, "unavailable") || strings.Contains(message, "earlier upstream failed") {
						t.Errorf("cached success retained previous request's fallback: %q", message)
					}
				} else if fallback {
					assertConsolePhrases(t, message, "Attempt 1", "unavailable", "unexpected HTTP status", "HTTP 503", "Attempt 2", "2 attempts", "earlier upstream failed")
				} else {
					assertConsolePhrases(t, message, "Attempt 1", "1 attempt.")
				}
				for _, secret := range []string{
					"server-id-secret", "ip-secret", "credential-user-secret", "credential-password-secret", "private-path-secret",
					"config-query-secret", "fragment-secret", "response-body-secret", "opaque+/=", "signed+/=", winner.server.URL,
				} {
					if strings.Contains(message, secret) {
						t.Errorf("text log leaked %q: %q", secret, message)
					}
				}
			}
		})
	}
}

func TestConsolePresentationDoesNotChangeJSONEvent(t *testing.T) {
	attrs := consoleSummaryTestAttrs("success", 2, true, true)
	var text, structured bytes.Buffer
	for _, test := range []struct {
		format string
		output *bytes.Buffer
	}{
		{"text", &text}, {"json", &structured},
	} {
		logger, err := newLogger(test.output, "info", test.format)
		if err != nil {
			t.Fatal(err)
		}
		logger.LogAttrs(context.Background(), slog.LevelInfo, "authentication_completed", attrs...)
	}
	assertHumanConsoleMessage(t, text.String())
	assertConsolePhrases(t, text.String(), "Player Steve authenticated", "2 attempts", "cached upstream was selected first", "earlier upstream failed", "Request request-42.")
	var record map[string]any
	if err := json.Unmarshal(structured.Bytes(), &record); err != nil {
		t.Fatalf("JSON log is not structured JSON: %v", err)
	}
	for key, want := range map[string]any{
		"msg": "authentication_completed", "level": "INFO", "request_id": "request-42", "username": "Steve",
		"outcome": "success", "status": float64(200), "attempts": float64(2), "cache_hit": true,
		"had_upstream_error": true, "duration_ms": 118.6, "upstream": "Mojang", "upstream_host": "sessionserver.mojang.com",
	} {
		if record[key] != want {
			t.Errorf("JSON field %s = %#v, want %#v", key, record[key], want)
		}
	}
	if strings.Contains(structured.String(), "Player Steve authenticated") {
		t.Errorf("JSON message was replaced by console prose: %q", structured.String())
	}
}
