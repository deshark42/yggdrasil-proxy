package main

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

const (
	maxUpstreamAttempts = 3
	maxUpstreamRetries  = maxUpstreamAttempts - 1
	upstreamRetryDelay  = 200 * time.Millisecond
)

func retryableNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return false
	}
	var address *net.AddrError
	var invalidAddress net.InvalidAddrError
	if errors.As(err, &address) || errors.As(err, &invalidAddress) {
		return false
	}
	switch requestErrorReason(err) {
	case "upstream_timeout", "connection_error", "dns_error":
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func waitForUpstreamRetry(ctx context.Context, retry int) bool {
	timer := time.NewTimer(upstreamRetryDelay << (retry - 1))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
