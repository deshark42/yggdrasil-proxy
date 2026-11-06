package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var threshold slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		threshold = slog.LevelInfo
	case "debug":
		threshold = slog.LevelDebug
	case "warn":
		threshold = slog.LevelWarn
	case "error":
		threshold = slog.LevelError
	default:
		return nil, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error")
	}
	opts := &slog.HandlerOptions{Level: threshold}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		handler = newConsoleHandler(w, threshold)
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("LOG_FORMAT must be text or json")
	}
	return slog.New(handler), nil
}

func logValue(value string) string {
	if len(value) > 128 {
		return value[:128] + "..."
	}
	return value
}

func upstreamLogFields(server AuthServer) []any {
	host := ""
	if parsed, err := parseAuthServerURL(server.URL); err == nil {
		host = parsed.Host
	}
	return []any{"upstream", logValue(server.Name), "upstream_host", host}
}

// Never log raw transport errors: url.Error contains the full authentication URL.
func requestErrorReason(err error) string {
	if errors.Is(err, context.Canceled) {
		return "upstream_canceled"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "upstream_timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error"
	}
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certificate x509.CertificateInvalidError
	if errors.As(err, &verification) || errors.As(err, &record) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &certificate) {
		return "tls_error"
	}
	var connection *net.OpError
	if errors.As(err, &connection) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return "connection_error"
	}
	return "request_error"
}

func durationMillis(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}

type authenticationLog struct {
	logger           *slog.Logger
	started          time.Time
	outcome          string
	status           int
	attempts         int
	cacheHit         bool
	hadUpstreamError bool
	upstream         *AuthServer
}

func newAuthenticationLog(w http.ResponseWriter, username string) *authenticationLog {
	requestID := rand.Text()
	w.Header().Set("X-Request-ID", requestID)
	return &authenticationLog{
		logger:  slog.Default().With("request_id", requestID, "username", logValue(username)),
		started: time.Now(),
		outcome: "internal_error",
	}
}

func (entry *authenticationLog) complete(outcome string, status int) {
	entry.outcome, entry.status = outcome, status
}

func (entry *authenticationLog) stopped(w http.ResponseWriter, requestContext, authContext context.Context) bool {
	if !authenticationStopped(w, requestContext, authContext) {
		return false
	}
	if requestContext.Err() != nil {
		entry.complete("canceled", 0)
	} else {
		entry.complete("timeout", http.StatusBadGateway)
	}
	return true
}

func (entry *authenticationLog) attempt(ctx context.Context, server AuthServer, result authResult, started time.Time) {
	level, outcome := slog.LevelDebug, "upstream_error"
	switch result.outcome {
	case authSuccess:
		outcome = "success"
	case authNoMatch:
		outcome = "no_match"
	case upstreamError:
		if ctx.Err() == nil {
			entry.hadUpstreamError = true
			level = slog.LevelWarn
		}
	}
	if !entry.logger.Enabled(context.Background(), level) {
		return
	}
	attrs := upstreamLogFields(server)
	attrs = append(attrs, "attempt", entry.attempts, "cached", entry.cacheHit && entry.attempts == 1,
		"status", result.status, "outcome", outcome, "duration_ms", durationMillis(started))
	if result.reason != "" {
		attrs = append(attrs, "reason", result.reason)
	}
	entry.logger.Log(context.Background(), level, "upstream_attempt", attrs...)
}

func (entry *authenticationLog) finish() {
	level := slog.LevelWarn
	switch entry.outcome {
	case "success", "no_match", "canceled":
		level = slog.LevelInfo
	case "internal_error":
		level = slog.LevelError
	}
	attrs := []any{"outcome", entry.outcome, "status", entry.status,
		"attempts", entry.attempts, "cache_hit", entry.cacheHit,
		"had_upstream_error", entry.hadUpstreamError, "duration_ms", durationMillis(entry.started)}
	if entry.upstream != nil {
		attrs = append(attrs, upstreamLogFields(*entry.upstream)...)
	}
	entry.logger.Log(context.Background(), level, "authentication_completed", attrs...)
}
