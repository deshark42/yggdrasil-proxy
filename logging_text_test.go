package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestConsoleHandlerLayout(t *testing.T) {
	var output bytes.Buffer
	handler := newConsoleHandler(&output, slog.LevelInfo)
	record := slog.NewRecord(time.Date(2026, 11, 6, 14, 30, 5, 120000000, time.FixedZone("CST", 8*3600)), slog.LevelInfo, "authentication_completed", 0)
	record.AddAttrs(slog.String("username", "Steve"), slog.String("outcome", "success"), slog.String("upstream", "Mojang"), slog.Int("attempts", 1), slog.Float64("duration_ms", 118.6))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := "2026-11-06 14:30:05.120 INFO  Player Steve authenticated via Mojang in 118.6 ms after 1 attempt.\n"
	if output.String() != want {
		t.Fatalf("text log = %q, want %q", output.String(), want)
	}
	output.Reset()
	if err := handler.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelWarn, "No fields", 0)); err != nil {
		t.Fatal(err)
	}
	if output.String() != "WARN  No fields\n" {
		t.Fatalf("log without time or attributes = %q", output.String())
	}
}

func TestConsoleHandlerEscapesSingleLine(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(newConsoleHandler(&output, slog.LevelDebug))
	logger.Info("unsafe\nmessage\r\x1b", "username", "name\nforged=event", "space", "two words", "quote", "a\"b", "empty", "", "中文", "玩家", "a=b", "c\\d")
	line := output.String()
	if strings.Count(line, "\n") != 1 || strings.ContainsAny(line, "\r\x1b") {
		t.Fatalf("log contains unescaped control characters: %q", line)
	}
	for _, field := range []string{`"unsafe\nmessage\r\x1b"`, `username: "name\nforged=event"`, `space: "two words"`, `quote: "a\"b"`, `empty: ""`, "中文: 玩家", `a=b: "c\\d"`} {
		if !strings.Contains(line, field) {
			t.Errorf("log %q does not contain %q", line, field)
		}
	}
}

type consoleTestValue struct{}

func (consoleTestValue) LogValue() slog.Value { return slog.StringValue("resolved") }

func TestConsoleHandlerDerivedLoggersAndGroups(t *testing.T) {
	var output bytes.Buffer
	root := slog.New(newConsoleHandler(&output, slog.LevelDebug))
	base := root.With("request_id", "r1")
	grouped := base.WithGroup("auth").With("username", "Steve")
	grouped.WithGroup("").Info("grouped", slog.Group("response", "status", 200), slog.Group("", "inline", true), "value", consoleTestValue{}, slog.Attr{}, slog.Group("empty"))
	base.Info("base")
	root.Info("root")
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("logs = %q", output.String())
	}
	want := " (request id: r1; auth username: Steve; auth response status: 200; auth inline: true; auth value: resolved)"
	if !strings.HasSuffix(lines[0], want) || !strings.HasSuffix(lines[1], " (request id: r1)") || strings.Contains(lines[2], " (") {
		t.Fatalf("derived logger fields leaked or groups changed: %q", output.String())
	}
}

func TestConsoleHandlerConcurrentDerivedLoggers(t *testing.T) {
	var output bytes.Buffer
	root := slog.New(newConsoleHandler(&output, slog.LevelInfo))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger := root.With("worker", i).WithGroup("auth")
			for j := 0; j < 10; j++ {
				logger.Info("event", "iteration", j)
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	seen := make(map[string]bool)
	for _, line := range lines {
		_, fields, ok := strings.Cut(line, "event (")
		fields = strings.TrimSuffix(fields, ")")
		if !ok || seen[fields] {
			t.Fatalf("interleaved or repeated log: %q", line)
		}
		seen[fields] = true
	}
	for i := 0; i < 20; i++ {
		for j := 0; j < 10; j++ {
			if !seen[fmt.Sprintf("worker: %d; auth iteration: %d", i, j)] {
				t.Fatalf("missing event for worker=%d iteration=%d", i, j)
			}
		}
	}
	if len(lines) != 200 {
		t.Fatalf("line count = %d, want 200", len(lines))
	}
}

type consoleShortWriter struct{}

func (consoleShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestConsoleHandlerReportsShortWrites(t *testing.T) {
	err := newConsoleHandler(consoleShortWriter{}, slog.LevelInfo).Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "event", 0))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Handle error = %v, want io.ErrShortWrite", err)
	}
}

func TestConsoleHandlerEscapesInvalidText(t *testing.T) {
	var output bytes.Buffer
	handler := newConsoleHandler(&output, slog.LevelInfo)
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "authentication_completed", 0)
	record.AddAttrs(slog.String("outcome", "success"), slog.String("username", "Steve\nforged\xff"), slog.String("upstream", "Mojang\r\x1b"), slog.String("request_id", "id\nforged"))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	line := output.String()
	if strings.Count(line, "\n") != 1 || strings.ContainsAny(line, "\r\x1b") || !utf8.ValidString(line) {
		t.Fatalf("unsafe log line: %q", line)
	}
	for _, escaped := range []string{`Steve\nforged\xff`, `Mojang\r\x1b`, `id\nforged`} {
		if !strings.Contains(line, escaped) {
			t.Fatalf("missing escaped value %q in %q", escaped, line)
		}
	}
}

type consoleFailedWriter struct{}

func (consoleFailedWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestConsoleHandlerReturnsWriterErrors(t *testing.T) {
	handler := newConsoleHandler(consoleFailedWriter{}, slog.LevelInfo)
	if err := handler.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelError, "event", 0)); err == nil || err.Error() != "write failed" {
		t.Fatalf("Handle error = %v, want writer error", err)
	}
}
