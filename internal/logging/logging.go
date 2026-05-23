// Package logging configures the process-wide structured logger.
//
// Every binary here logs to stderr and something else collects it -- CloudWatch
// for the Fargate components, a terminal for local development. That split is
// the whole reason the format is a setting: JSON is queryable and unreadable,
// text is the reverse, and neither is right in both places.
//
// Structured rather than formatted for three reasons that arrive together with
// multi-tenancy:
//
//   - Every line needs tenant context. "session %s created in workspace %s"
//     cannot be filtered by tenant; attributes can, and "what did this customer
//     see" is otherwise unanswerable.
//   - The relay becomes its own binary, so one user action spans three
//     processes. A request id as an attribute replaces three greps with one
//     query.
//   - Credentials redact themselves through slog.LogValuer (internal/creds),
//     which only works if credentials reach a logger that consults it. A
//     Printf-formatted %s does not.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Environment variables, read once at Setup. Deliberately not flags: every
// binary needs them and none of them is worth a flag on all four.
const (
	EnvFormat = "LEMUL_LOG_FORMAT" // "text" (default) | "json"
	EnvLevel  = "LEMUL_LOG_LEVEL"  // debug | info (default) | warn | error
)

// Setup installs the default logger and returns it.
//
// service names the binary and is attached to every line, because once the
// relay is split out three processes write into one log group and "which one
// said this" stops being obvious.
//
// The default is text: the format that is wrong in production is merely ugly,
// while the format that is wrong locally makes development actively worse, so
// the failure to configure should land on the side that is deployed
// deliberately. image/entrypoint.sh and the task definitions set json.
func Setup(service string) *slog.Logger {
	l := build(os.Stderr, service, os.Getenv(EnvFormat), os.Getenv(EnvLevel))
	slog.SetDefault(l)
	return l
}

// build is Setup without the two global side effects -- the destination and
// slog.SetDefault -- so the format and level rules can be tested rather than
// eyeballed by starting a binary.
func build(w io.Writer, service, format, lvl string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(lvl)}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h).With("service", service)
}

func parseLevel(v string) slog.Level {
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Fatal logs at error level and exits.
//
// slog has no Fatal, correctly -- it is a logging concern only by habit. It
// exists here so the four mains do not each reinvent it, and so a startup
// failure is one structured line rather than a stray log.Fatalf that the
// handler never sees.
func Fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// Writer adapts an io.Writer-based logger to slog.
//
// yamux takes an io.Writer and writes pre-formatted lines to it. Left on stderr
// those bypass the handler entirely: unlevelled, unstructured, and untouched by
// the JSON format everything else obeys. This routes them through slog at a
// fixed level, which also makes yamux's routine "[ERR] ... unexpected EOF" on
// every clean disconnect suppressible rather than permanent noise.
func Writer(l *slog.Logger, lv slog.Level) io.Writer {
	if l == nil {
		l = slog.Default()
	}
	return &logWriter{l: l, lv: lv}
}

type logWriter struct {
	l  *slog.Logger
	lv slog.Level
}

func (w *logWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		// LogAttrs with a nil context: this is called from library goroutines
		// that have none, and inventing one would attach nothing.
		w.l.Log(context.Background(), w.lv, msg)
	}
	return len(p), nil
}
