package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

type Options struct {
	Level     string
	Format    string
	AddSource bool
	Output    io.Writer
	FilePath  string
	Rotate    RotateOptions
}

type RotateOptions struct {
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
}

func New(opts Options) (*slog.Logger, error) {
	var lv slog.Level
	switch opts.Level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}

	out := opts.Output
	if out == nil {
		var err error
		out, err = outputWriter(opts)
		if err != nil {
			return nil, err
		}
	}

	handlerOpts := &slog.HandlerOptions{
		Level:     lv,
		AddSource: opts.AddSource,
	}

	var h slog.Handler
	if opts.Format == "text" {
		h = slog.NewTextHandler(out, handlerOpts)
	} else {
		h = slog.NewJSONHandler(out, handlerOpts)
	}
	return slog.New(h), nil
}

func outputWriter(opts Options) (io.Writer, error) {
	if opts.FilePath == "" {
		return os.Stdout, nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.FilePath), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	return &lumberjack.Logger{
		Filename:   opts.FilePath,
		MaxSize:    opts.Rotate.MaxSizeMB,
		MaxBackups: opts.Rotate.MaxBackups,
		MaxAge:     opts.Rotate.MaxAgeDays,
		Compress:   opts.Rotate.Compress,
	}, nil
}
