package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

const (
	logDirEnvVar  = "BENCHTOP_LOG_DIR"
	logFileEnvVar = "BENCHTOP_LOG_FILE"
)

// setupLogging configures the standard library logger to write to both stderr and a
// persistent log file, enabling post-run inspection when the TUI clears the terminal.
func setupLogging() (func() error, string, error) {
	path, err := resolveLogFilePath()
	if err != nil {
		return nil, "", err
	}

	logFile, err := openLogFilePath(path)
	if err != nil {
		return nil, "", err
	}

	writers := []io.Writer{logFile, os.Stderr}
	log.SetOutput(io.MultiWriter(writers...))
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	closeFn := func() error {
		if err := logFile.Close(); err != nil {
			return fmt.Errorf("closing log file: %w", err)
		}
		return nil
	}
	return closeFn, path, nil
}

func resolveLogFilePath() (string, error) {
	if path := os.Getenv(logFileEnvVar); path != "" {
		if err := ensureLogDir(filepath.Dir(path)); err != nil {
			return "", err
		}
		return path, nil
	}

	if dir := os.Getenv(logDirEnvVar); dir != "" {
		if err := ensureLogDir(dir); err != nil {
			return "", err
		}
		return filepath.Join(dir, "benchtop-cli.log"), nil
	}

	exePath, err := os.Executable()
	if err != nil {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return "", fmt.Errorf("determining log directory: %w", err)
		}
		if err := ensureLogDir(cwd); err != nil {
			return "", err
		}
		return filepath.Join(cwd, "benchtop-cli.log"), nil
	}

	dir := filepath.Dir(exePath)
	if err := ensureLogDir(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, "benchtop-cli.log"), nil
}

func ensureLogDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("log directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating log dir %q: %w", dir, err)
	}
	return nil
}

func openLogFilePath(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening log file %q: %w", path, err)
	}
	return f, nil
}
