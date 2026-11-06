package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type consoleHandler struct {
	writer io.Writer
	mu     *sync.Mutex
	level  slog.Level
	attrs  []slog.Attr
	group  string
}

func newConsoleHandler(w io.Writer, level slog.Level) *consoleHandler {
	return &consoleHandler{writer: w, mu: new(sync.Mutex), level: level}
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *consoleHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := append([]slog.Attr(nil), h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = appendConsoleAttrs(attrs, h.group, attr)
		return true
	})
	var line strings.Builder
	if !record.Time.IsZero() {
		line.WriteString(record.Time.Format("2006-01-02 15:04:05.000"))
		line.WriteByte(' ')
	}
	fmt.Fprintf(&line, "%-5s %s\n", record.Level.String(), consoleMessage(consoleSentence(record.Message, attrs)))
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := io.WriteString(h.writer, line.String())
	if err == nil && n < line.Len() {
		return io.ErrShortWrite
	}
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, attr := range attrs {
		clone.attrs = appendConsoleAttrs(clone.attrs, h.group, attr)
	}
	return &clone
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.group += name + "."
	return &clone
}

func appendConsoleAttrs(attrs []slog.Attr, group string, attr slog.Attr) []slog.Attr {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return attrs
	}
	if attr.Value.Kind() == slog.KindGroup {
		if attr.Key != "" {
			group += attr.Key + "."
		}
		for _, child := range attr.Value.Group() {
			attrs = appendConsoleAttrs(attrs, group, child)
		}
		return attrs
	}
	attr.Key = group + attr.Key
	return append(attrs, attr)
}

func consoleField(value string) string {
	if value == "" || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool {
		return r == '=' || r == '"' || r == '\\' || unicode.IsSpace(r) || !unicode.IsPrint(r)
	}) >= 0 {
		return strconv.Quote(value)
	}
	return value
}

func consoleMessage(message string) string {
	if !utf8.ValidString(message) || strings.IndexFunc(message, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return strconv.Quote(message)
	}
	return message
}
