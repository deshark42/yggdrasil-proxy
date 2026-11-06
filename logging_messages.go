package main

import (
	"fmt"
	"log/slog"
	"strings"
)

type consoleFields map[string]slog.Value

func (f consoleFields) text(key string) string { return consoleField(f[key].String()) }

func (f consoleFields) number(key string) float64 {
	switch value := f[key]; value.Kind() {
	case slog.KindInt64:
		return float64(value.Int64())
	case slog.KindUint64:
		return float64(value.Uint64())
	case slog.KindFloat64:
		return value.Float64()
	}
	return 0
}

func (f consoleFields) flag(key string) bool {
	value := f[key]
	return value.Kind() == slog.KindBool && value.Bool()
}

func (f consoleFields) upstream() string {
	name := f.text("upstream")
	if host, ok := f["upstream_host"]; ok && host.String() != "" {
		name += " (" + consoleField(host.String()) + ")"
	}
	return name
}

func consoleSentence(message string, attrs []slog.Attr) string {
	fields := make(consoleFields, len(attrs))
	for _, attr := range attrs {
		fields[attr.Key] = attr.Value
	}
	switch message {
	case "authentication_completed":
		return consoleAuthentication(fields)
	case "upstream_attempt":
		return consoleAttempt(fields)
	case "upstream_configured":
		return fmt.Sprintf("Configured upstream %s with priority %.0f and a %.0f s timeout.", fields.upstream(), fields.number("priority"), fields.number("timeout_seconds"))
	case "proxy_starting":
		return fmt.Sprintf("Starting Yggdrasil Proxy on %s with %.0f upstreams and a %.0f s authentication limit. Cache holds up to %.0f players for %.0f s and is cleaned every %.0f s.", fields.text("address"), fields.number("upstreams"), fields.number("auth_timeout_seconds"), fields.number("cache_max_entries"), fields.number("cache_ttl_seconds"), fields.number("cache_cleanup_interval_seconds"))
	case "default_config_created":
		return fmt.Sprintf("Created default configuration at %s.", fields.text("config_path"))
	case "logging_configuration_failed":
		return fmt.Sprintf("Cannot configure logging: %s.", fields.text("error"))
	case "configuration_failed":
		return fmt.Sprintf("Cannot load configuration from %s: %s.", fields.text("config_path"), fields.text("error"))
	case "server_failed":
		return fmt.Sprintf("HTTP server on %s failed: %s.", fields.text("address"), fields.text("error"))
	}
	if len(attrs) == 0 {
		return message
	}
	parts := make([]string, 0, len(attrs))
	for _, attr := range attrs {
		label := strings.NewReplacer("_", " ", ".", " ").Replace(attr.Key)
		parts = append(parts, consoleMessage(label)+": "+consoleField(attr.Value.String()))
	}
	return consoleMessage(message) + " (" + strings.Join(parts, "; ") + ")"
}

func consoleAuthentication(f consoleFields) string {
	player, duration := f.text("username"), f.number("duration_ms")
	var sentence string
	switch f["outcome"].String() {
	case "success":
		sentence = fmt.Sprintf("Player %s authenticated via %s", player, f.upstream())
	case "no_match":
		sentence = fmt.Sprintf("No upstream authenticated player %s (HTTP 204)", player)
	case "upstream_error":
		sentence = fmt.Sprintf("Could not authenticate player %s because an upstream failed (HTTP 502)", player)
	case "invalid_request":
		sentence = fmt.Sprintf("Rejected authentication request for player %s: username or server ID is missing (HTTP 400)", player)
		if f["username"].String() == "" {
			sentence = "Rejected authentication request: username or server ID is missing (HTTP 400)"
		}
	case "timeout":
		sentence = fmt.Sprintf("Authentication for player %s exceeded the total time limit (HTTP 502)", player)
	case "canceled":
		sentence = fmt.Sprintf("Authentication for player %s was canceled by the client", player)
	case "response_write_error":
		sentence = fmt.Sprintf("Could not deliver the response to player %s after authentication via %s", player, f.upstream())
	default:
		sentence = fmt.Sprintf("Authentication for player %s stopped because of an internal error", player)
	}
	attempts := f.number("attempts")
	plural := "attempts"
	if attempts == 1 {
		plural = "attempt"
	}
	sentence += fmt.Sprintf(" in %.1f ms after %.0f %s.", duration, attempts, plural)
	if f["outcome"].String() == "canceled" {
		sentence += " No response was sent."
	}
	if f.flag("cache_hit") {
		sentence += " The cached upstream was selected first."
	}
	if f.flag("had_upstream_error") && f["outcome"].String() != "upstream_error" {
		sentence += " An earlier upstream failed."
	}
	return sentence + consoleRequest(f)
}

func consoleAttempt(f consoleFields) string {
	verb := "failed"
	switch f["outcome"].String() {
	case "success":
		verb = "authenticated the player"
	case "no_match":
		verb = "did not authenticate the player"
	default:
		switch f["reason"].String() {
		case "upstream_timeout":
			verb = "timed out"
		case "upstream_canceled":
			verb = "was canceled"
		case "dns_error":
			verb = "failed to resolve the upstream host"
		case "connection_error":
			verb = "encountered an upstream connection error"
		case "tls_error":
			verb = "failed TLS verification or negotiation"
		case "unexpected_status":
			verb = "received an unexpected HTTP status"
		case "response_read_error":
			verb = "failed to read the profile response"
		case "profile_too_large":
			verb = "received a profile larger than 1 MiB"
		case "invalid_profile_json":
			verb = "received invalid profile JSON"
		case "invalid_profile":
			verb = "received a profile with a missing or invalid ID or name"
		case "invalid_profile_uuid":
			verb = "received a profile with an invalid UUID"
		case "invalid_upstream_url":
			verb = "could not use the configured upstream address"
		case "request_build_error":
			verb = "could not create the upstream request"
		}
	}
	sentence := fmt.Sprintf("Attempt %.0f for player %s via %s %s after %.1f ms", f.number("attempt"), f.text("username"), f.upstream(), verb, f.number("duration_ms"))
	if status := f.number("status"); status != 0 {
		sentence += fmt.Sprintf(" (HTTP %.0f).", status)
	} else {
		sentence += "; no HTTP response was received."
	}
	if f.flag("cached") {
		sentence += " This upstream was selected from the cache."
	}
	return sentence + consoleRequest(f)
}

func consoleRequest(f consoleFields) string {
	if id, ok := f["request_id"]; ok && id.String() != "" {
		return " Request " + consoleField(id.String()) + "."
	}
	return ""
}
