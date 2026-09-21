// Package logger provides a leveled, structured logger with an enable/disable
// switch. It wraps log/slog and additionally satisfies the stdlib printer
// surface (Printf/Println) so existing layers can switch over without changes
// to call sites. Disabled loggers discard everything below no level.
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config controls a Logger instance.
type Config struct {
	// Enabled toggles output entirely. When false the logger is a no-op.
	Enabled bool
	// Level is one of debug, info, warn, error or disabled. Empty means info.
	Level string
	// Format is "text" or "json". Empty means text.
	Format string
	// Out is the destination. Defaults to os.Stdout.
	Out io.Writer
}

// Logger is the structured logger.
type Logger struct {
	sl      *slog.Logger
	enabled bool
}

// New builds a Logger from cfg.
func New(cfg Config) *Logger {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(cfg.Level)) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "disabled", "off", "none":
		cfg.Enabled = false
	}

	if !cfg.Enabled {
		level = slog.Level(1 << 30)
	}
	out := cfg.Out
	if out == nil {
		out = os.Stdout
	}

	var h slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(strings.TrimSpace(cfg.Format), "json") {
		h = slog.NewJSONHandler(out, opts)
	} else {
		h = slog.NewTextHandler(out, opts)
	}
	return &Logger{sl: slog.New(h), enabled: cfg.Enabled}
}

// Nop returns a fully disabled logger.
func Nop() *Logger {
	return &Logger{sl: slog.New(slog.NewTextHandler(io.Discard, nil)), enabled: false}
}

// Enabled reports whether the given level would currently be emitted.
func (l *Logger) Enabled(lvl slog.Level) bool {
	if l == nil || !l.enabled {
		return false
	}
	return l.sl.Enabled(context.Background(), lvl)
}

// Slog exposes the underlying slog.Logger for attribute-style logging.
func (l *Logger) Slog() *slog.Logger {
	if l == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return l.sl
}

// With returns a child logger bound to the given attributes.
func (l *Logger) With(args ...any) *Logger {
	if l == nil {
		return l
	}
	return &Logger{sl: l.sl.With(args...), enabled: l.enabled}
}

// Debugf logs a formatted debug message.
func (l *Logger) Debugf(format string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Debug(fmt.Sprintf(format, args...))
}

// Infof logs a formatted info message.
func (l *Logger) Infof(format string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Info(fmt.Sprintf(format, args...))
}

// Warnf logs a formatted warning message.
func (l *Logger) Warnf(format string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Warn(fmt.Sprintf(format, args...))
}

// Errorf logs a formatted error message.
func (l *Logger) Errorf(format string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Error(fmt.Sprintf(format, args...))
}

// Debug logs a debug message with structured attributes (key-value pairs).
func (l *Logger) Debug(msg string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Debug(msg, args...)
}

// Info logs an info message with structured attributes (key-value pairs).
func (l *Logger) Info(msg string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Info(msg, args...)
}

// Warn logs a warning message with structured attributes (key-value pairs).
func (l *Logger) Warn(msg string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Warn(msg, args...)
}

// Error logs an error message with structured attributes (key-value pairs).
func (l *Logger) Error(msg string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Error(msg, args...)
}

// Printf is the stdlib-compatible info-level formatter.
func (l *Logger) Printf(format string, args ...any) {
	l.Infof(format, args...)
}

// Println is the stdlib-compatible info-level line printer.
func (l *Logger) Println(args ...any) {
	if l == nil {
		return
	}
	l.sl.Info(fmt.Sprintln(args...))
}

// Fatalf logs a formatted error message and exits with status 1.
func (l *Logger) Fatalf(format string, args ...any) {
	l.Errorf(format, args...)
	os.Exit(1)
}
