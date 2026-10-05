package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	Enabled bool

	Level string

	Format string

	Out io.Writer
}

type Logger struct {
	sl      *slog.Logger
	enabled bool
}

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

func Nop() *Logger {
	return &Logger{sl: slog.New(slog.NewTextHandler(io.Discard, nil)), enabled: false}
}

func (l *Logger) Enabled(lvl slog.Level) bool {
	if l == nil || !l.enabled {
		return false
	}
	return l.sl.Enabled(context.Background(), lvl)
}

func (l *Logger) Slog() *slog.Logger {
	if l == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return l.sl
}

func (l *Logger) With(args ...any) *Logger {
	if l == nil {
		return l
	}
	return &Logger{sl: l.sl.With(args...), enabled: l.enabled}
}

func (l *Logger) Debugf(format string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Debug(fmt.Sprintf(format, args...))
}

func (l *Logger) Infof(format string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Info(fmt.Sprintf(format, args...))
}

func (l *Logger) Warnf(format string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Warn(fmt.Sprintf(format, args...))
}

func (l *Logger) Errorf(format string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Error(fmt.Sprintf(format, args...))
}

func (l *Logger) Debug(msg string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Debug(msg, args...)
}

func (l *Logger) Info(msg string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Info(msg, args...)
}

func (l *Logger) Warn(msg string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Warn(msg, args...)
}

func (l *Logger) Error(msg string, args ...any) {
	if l == nil {
		return
	}
	l.sl.Error(msg, args...)
}

func (l *Logger) Printf(format string, args ...any) {
	l.Infof(format, args...)
}

func (l *Logger) Println(args ...any) {
	if l == nil {
		return
	}
	l.sl.Info(fmt.Sprintln(args...))
}

func (l *Logger) Fatalf(format string, args ...any) {
	l.Errorf(format, args...)
	os.Exit(1)
}
