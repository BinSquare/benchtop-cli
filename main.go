package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rivo/tview"
)

// commandRunner executes a sub-command using the provided CLI arguments.
type commandRunner func([]string) error

// commandHandlers maps supported command names (including aliases) to their implementations.
var commandHandlers = map[string]commandRunner{
	"benchtop":          runBenchtopCommand,



}

var defaultCommand = "benchtop"

// Config captures dependencies required to start the TUI.
type Config struct {
	Title           string
	RefreshInterval time.Duration
	Collector       CollectorController
}

// CollectorController describes the subset of the collector manager needed by the UI.
type CollectorController interface {
	Run(ctx context.Context, emit EmitFunc) error
}

const (
	defaultTitle           = "benchtop"
	defaultRefreshInterval = time.Second
)

func sendEnvelope(ctx context.Context, out chan<- Envelope, env Envelope) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- env:
		return true
	}
}

// Run boots the TUI event loop with default configuration.
func Run() error {
	return RunWithConfig(Config{})
}

// RunWithConfig allows callers to customize the TUI runtime.
func RunWithConfig(cfg Config) error {
	if cfg.Title == "" {
		cfg.Title = defaultTitle
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if cfg.Collector == nil {
		// Default to real streams: process metrics, and powermetrics if available
		var streams []Stream
		streams = append(streams, StreamProcesses(ProcessConfig{}))
		
		// Add powermetrics stream if enabled
		if os.Getenv("BENCHTOP_DISABLE_POWERMETRICS") == "" {
			helperCfg := HelperConfig{
				SampleWindow: cfg.RefreshInterval, // Use the same refresh interval
			}
			streams = append(streams, StreamHelper(helperCfg))
		}
		
		cfg.Collector = NewManager(streams...)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	envCh := make(chan Envelope, 128)
	errCh := make(chan error, 1)

	go runCollector(ctx, cfg.Collector, envCh, errCh)

	// Create tview application
	app := tview.NewApplication()

	// Create the main UI
	ui := NewTUIView(app, cfg.Title, cfg.RefreshInterval)
	go ui.Start(envCh, errCh)

	// Run the application
	if err := app.SetRoot(ui.GetLayout(), true).EnableMouse(true).Run(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func runCollector(ctx context.Context, ctrl CollectorController, envCh chan<- Envelope, errCh chan<- error) {
	defer close(envCh)
	defer close(errCh)

	err := ctrl.Run(ctx, func(env Envelope) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case envCh <- env:
			return nil
		}
	})

	if err != nil && !errors.Is(err, context.Canceled) {
		errCh <- err
	}
}

func runBenchtopCommand(_ []string) error {
	var streams []Stream
	
	// Add process stream (doesn't require special privileges)
	streams = append(streams, StreamProcesses(ProcessConfig{}))
	
	// Add helper stream (powermetrics) only if configured and available
	if isPowermetricsEnabled() {
		helperCfg := HelperConfig{}
		if path := os.Getenv("BENCHTOP_POWERMETRICS_PATH"); path != "" {
			helperCfg.PowermetricsPath = path
		}
		if args := os.Getenv("BENCHTOP_POWERMETRICS_ARGS"); args != "" {
			helperCfg.PowermetricsArgs = strings.Fields(args)
		}
		if interval := os.Getenv("BENCHTOP_POWERMETRICS_WINDOW"); interval != "" {
			if d, err := time.ParseDuration(interval); err == nil {
				helperCfg.SampleWindow = d
			}
		}
		streams = append(streams, StreamHelper(helperCfg))
	}
	
	manager := NewManager(streams...)

	cfg := Config{
		Title:           "benchtop",
		RefreshInterval: 750 * time.Millisecond,
		Collector:       manager,
	}

	return RunWithConfig(cfg)
}

// isPowermetricsEnabled checks if powermetrics should be enabled
func isPowermetricsEnabled() bool {
	// Check if user explicitly disabled powermetrics
	if os.Getenv("BENCHTOP_DISABLE_POWERMETRICS") != "" {
		return false
	}
	return true
}

func main() {
	cleanup, logPath, err := setupLogging()
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchtop: unable to initialize logging: %v\n", err)
	} else {
		defer func() {
			if err := cleanup(); err != nil {
				fmt.Fprintf(os.Stderr, "benchtop: %v\n", err)
			}
		}()
		log.Printf("logging initialized; writing to %s", logPath)
	}

	if err := dispatch(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		printUsage(os.Stderr)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no arguments provided")
	}

	if len(args) > 1 {
		if isHelpFlag(args[1]) {
			printUsage(os.Stdout)
			return nil
		}
	}

	if runner, ok := resolveCommand(filepath.Base(args[0])); ok {
		return runner(args[1:])
	}

	if len(args) > 1 {
		if runner, ok := resolveCommand(args[1]); ok {
			return runner(args[2:])
		}
	}

	if runner, ok := resolveCommand(defaultCommand); ok {
		return runner(args[1:])
	}

	return fmt.Errorf("unknown command %q", args[0])
}

func resolveCommand(name string) (commandRunner, bool) {
	clean := normalizeName(name)
	handler, ok := commandHandlers[clean]
	return handler, ok
}

func normalizeName(name string) string {
	n := strings.TrimSpace(name)
	n = strings.TrimSuffix(n, filepath.Ext(n))
	n = strings.ToLower(n)
	return n
}

func isHelpFlag(arg string) bool {
	switch arg {
	case "-h", "--help", "help":
		return true
	default:
		return false
	}
}

func printUsage(w *os.File) {
	fmt.Fprintf(w, "Available commands:\n")
	var names []string
	seen := make(map[string]struct{})
	for name := range commandHandlers {
		if strings.Contains(name, "-") {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %s\n", name)
	}
}
