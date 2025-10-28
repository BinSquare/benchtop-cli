package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	powermetrics "github.com/BinSquare/powermetrics-go"
)

const (
	defaultPowermetricsPath = "/usr/bin/powermetrics"
	defaultSampleWindow     = time.Second
)

// PowerConfig wraps the configuration for powermetrics integration
type PowerConfig struct {
	PowermetricsPath string
	PowermetricsArgs []string
	SampleWindow     time.Duration
}

var (
	defaultPowermetricsArgs = []string{
		"--samplers", "default",
		"--show-process-gpu",
		"-i", "1000", // sample interval in ms
	}

	procLineRegex    = regexp.MustCompile(`^pid\s+(\d+)\s+(.+?)\s+([0-9]+(?:\.[0-9]+)?)\s*(us|ms|s)(?:\s+\(([0-9]+(?:\.[0-9]+)?)\s*%\))?(?:\s+.*)?$`)
	numberExtractor  = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)`)
	samplerFallbacks = map[string]string{
		"smc": "smc++",
	}
	powermetricsDebug = os.Getenv("BENCHTOP_DEBUG_POWERMETRICS") != ""
)

// HelperConfig configures powermetrics integration.
type HelperConfig struct {
	PowermetricsPath string
	PowermetricsArgs []string
	SampleWindow     time.Duration
}



// StreamHelper launches powermetrics (or the synthetic generator) and emits GPU
// samples as a Stream.
func StreamHelper(cfg HelperConfig) Stream {
	if cfg.PowermetricsPath == "" {
		cfg.PowermetricsPath = defaultPowermetricsPath
	}
	if len(cfg.PowermetricsArgs) == 0 {
		cfg.PowermetricsArgs = append([]string(nil), defaultPowermetricsArgs...)
	}
	if cfg.SampleWindow <= 0 {
		cfg.SampleWindow = defaultSampleWindow
	} else {
		milliseconds := int64(cfg.SampleWindow.Seconds() * 1000)
		found := false
		for i := 0; i < len(cfg.PowermetricsArgs)-1; i++ {
			if cfg.PowermetricsArgs[i] == "-i" {
				cfg.PowermetricsArgs[i+1] = fmt.Sprintf("%d", milliseconds)
				found = true
				break
			}
		}
		if !found {
			cfg.PowermetricsArgs = append(cfg.PowermetricsArgs, "-i", fmt.Sprintf("%d", milliseconds))
		}
	}
	return &helperStream{config: cfg}
}

type helperStream struct {
	config HelperConfig
}

func (h *helperStream) Start(ctx context.Context) (<-chan Envelope, error) {
	out := make(chan Envelope, 128)
	go h.run(ctx, out)
	return out, nil
}

func (h *helperStream) run(ctx context.Context, out chan<- Envelope) {
	defer close(out)

	helperLogf("run loop entered")
	
	// Create powermetrics library config
	pmConfig := PowerConfig{
		PowermetricsPath: h.config.PowermetricsPath,
		PowermetricsArgs: h.config.PowermetricsArgs,
		SampleWindow:     h.config.SampleWindow,
	}

	// Run powermetrics with the new library
	metricsCh, err := runPowermetricsWithConfig(ctx, pmConfig)
	if err != nil {
		helperLogf("failed to start powermetrics: %v", err)
		msg := err.Error()
		if errors.Is(err, exec.ErrNotFound) {
			msg = fmt.Sprintf("powermetrics not found at %s", h.config.PowermetricsPath)
		}
		env := Envelope{
			Timestamp: time.Now(),
			Source:    SourceHelper,
			Payload: ErrorSample{
				Component: "powermetrics",
				Message:   msg,
			},
		}
		sendEnvelope(ctx, out, env)
		return
	}

	// Process metrics from the powermetrics library
	for {
		select {
		case <-ctx.Done():
			return
		case pmMetrics, ok := <-metricsCh:
			if !ok {
				return // Channel closed
			}
			
			// Convert powermetrics library metrics to internal format
			if pmMetrics.SystemSample != nil {
				env := Envelope{
					Timestamp: time.Now(),
					Source:    SourceHelper,
					Payload:   *pmMetrics.SystemSample,
				}
				if !sendEnvelope(ctx, out, env) {
					return
				}
			}
			
			for _, gpuSample := range pmMetrics.GPUProcessSamples {
				env := Envelope{
					Timestamp: time.Now(),
					Source:    SourceHelper,
					Payload:   gpuSample,
				}
				if !sendEnvelope(ctx, out, env) {
					return
				}
			}
			
			// Handle cluster information if available
			if len(pmMetrics.Clusters) > 0 {
				// For now, we'll log this information - in a future implementation,
				// we could extend the metrics model to include cluster information
				if powermetricsDebug {
					helperLogf("received cluster information:")
					for _, cluster := range pmMetrics.Clusters {
						helperLogf("  %s (%s): Online=%.2f%%, Freq=%.2fMHz", 
							cluster.Name, cluster.Type, cluster.OnlinePercent, cluster.HWActiveFreq)
					}
				}
			}
		}
	}
}

// powermetricsMetrics is a struct that mimics the powermetrics.Metrics struct for internal use
type powermetricsMetrics struct {
	SystemSample       *SystemSample
	GPUProcessSamples  []GPUProcessSample
	Clusters           []ClusterInfo
}

// runPowermetricsWithConfig executes powermetrics with the provided configuration
func runPowermetricsWithConfig(ctx context.Context, config PowerConfig) (<-chan powermetrics.Metrics, error) {
	// The powermetrics-go library may not directly support custom path in Config
	// For now, create parser with a custom configuration
	
	// Create powermetrics configuration
	pmConfig := powermetrics.Config{
		SampleWindow:     config.SampleWindow,
		PowermetricsArgs: config.PowermetricsArgs,
	}
	
	// Create parser with the config
	parser := powermetrics.NewParser(pmConfig)
	
	// Run the parser and return metrics channel
	return parser.Run(ctx)
}

func (h *helperStream) streamPowermetrics(ctx context.Context, out chan<- Envelope) error {
	if _, err := os.Stat(h.config.PowermetricsPath); err != nil {
		return err
	}

	args := append([]string(nil), h.config.PowermetricsArgs...)
	window := h.config.SampleWindow
	for {
		err := h.runPowermetricsOnce(ctx, out, args, window)
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		if sampler := detectUnsupportedSampler(err); sampler != "" {
			if updatedArgs, replacement, replaced := replaceSampler(args, sampler); replaced {
				args = updatedArgs
				h.config.PowermetricsArgs = updatedArgs
				h.notifySamplerChange(ctx, out, sampler, replacement)
				continue
			}
			updatedArgs, removed := removeSampler(args, sampler)
			if removed {
				args = updatedArgs
				h.config.PowermetricsArgs = updatedArgs
				h.notifySamplerRemoval(ctx, out, sampler)
				continue
			}
		}
		return err
	}
}

func (h *helperStream) runPowermetricsOnce(ctx context.Context, out chan<- Envelope, args []string, window time.Duration) error {
	cmd := exec.CommandContext(ctx, h.config.PowermetricsPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if powermetricsDebug {
		debugPowermetrics("launching %s %s", h.config.PowermetricsPath, strings.Join(args, " "))
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return err
	}

	emit := func(env Envelope) error {
		if env.Timestamp.IsZero() {
			env.Timestamp = time.Now()
		}
		if !sendEnvelope(ctx, out, env) {
			return context.Canceled
		}
		return nil
	}
	parser := newPowermetricsParser(window, emit)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if err := parser.handleLine(scanner.Text()); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return context.Canceled
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		msg := strings.TrimSpace(stderrBuf.String())
		if msg == "" {
			return waitErr
		}
		if strings.Contains(strings.ToLower(msg), "root") {
			msg = msg + "; run benchtop with sudo or grant it Full Disk Access"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func detectUnsupportedSampler(err error) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	lower := strings.ToLower(raw)
	idx := strings.Index(lower, "unrecognized sample")
	if idx == -1 {
		return ""
	}
	candidateRegion := raw[idx:]
	splitFn := func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ':', ',', ';':
			return true
		}
		return false
	}
	for _, token := range strings.FieldsFunc(candidateRegion, splitFn) {
		clean := strings.Trim(token, "'\"`")
		if clean == "" {
			continue
		}
		if isSamplerToken(clean) {
			debugPowermetrics("detected unsupported sampler %s from error: %s", clean, raw)
			return strings.ToLower(clean)
		}
	}
	return ""
}

func removeSampler(args []string, sampler string) ([]string, bool) {
	if sampler == "" {
		return args, false
	}
	target := strings.ToLower(sampler)
	argsCopy := append([]string(nil), args...)
	for i := 0; i < len(argsCopy); i++ {
		if argsCopy[i] != "--samplers" || i+1 >= len(argsCopy) {
			continue
		}
		samplers := strings.Split(argsCopy[i+1], ",")
		var filtered []string
		removed := false
		for _, s := range samplers {
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				continue
			}
			if strings.ToLower(trimmed) == target {
				removed = true
				continue
			}
			filtered = append(filtered, trimmed)
		}
		if !removed {
			continue
		}
		if len(filtered) == 0 {
			return args, false
		}
		argsCopy[i+1] = strings.Join(filtered, ",")
		return argsCopy, true
	}
	return args, false
}

func replaceSampler(args []string, sampler string) ([]string, string, bool) {
	replacement, ok := samplerFallbacks[strings.ToLower(sampler)]
	if !ok || replacement == "" {
		return args, "", false
	}
	argsCopy := append([]string(nil), args...)
	for i := 0; i < len(argsCopy); i++ {
		if argsCopy[i] != "--samplers" || i+1 >= len(argsCopy) {
			continue
		}
		samplers := strings.Split(argsCopy[i+1], ",")
		replaced := false
		for j, s := range samplers {
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				continue
			}
			if strings.EqualFold(trimmed, sampler) {
				samplers[j] = replacement
				replaced = true
			}
		}
		if !replaced {
			continue
		}
		seen := make(map[string]struct{}, len(samplers))
		var filtered []string
		for _, s := range samplers {
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				continue
			}
			key := strings.ToLower(trimmed)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			filtered = append(filtered, trimmed)
		}
		if len(filtered) == 0 {
			return args, "", false
		}
		argsCopy[i+1] = strings.Join(filtered, ",")
		return argsCopy, replacement, true
	}
	return args, "", false
}

func (h *helperStream) notifySamplerChange(ctx context.Context, out chan<- Envelope, from, to string) {
	msg := fmt.Sprintf("powermetrics sampler %s unsupported; retrying with %s", from, to)
	h.emitSamplerNotice(ctx, out, msg)
	debugPowermetrics(msg)
}

func (h *helperStream) notifySamplerRemoval(ctx context.Context, out chan<- Envelope, sampler string) {
	msg := fmt.Sprintf("powermetrics sampler %s unsupported; continuing without it", sampler)
	h.emitSamplerNotice(ctx, out, msg)
	debugPowermetrics(msg)
}

func (h *helperStream) emitSamplerNotice(ctx context.Context, out chan<- Envelope, msg string) {
	env := Envelope{
		Timestamp: time.Now(),
		Source:    SourceHelper,
		Payload: ErrorSample{
			Component: "powermetrics",
			Message:   msg,
		},
	}
	sendEnvelope(ctx, out, env)
}

func helperLogf(format string, args ...any) {
	log.Printf("[collector/helper] "+format, args...)
}

func debugPowermetrics(format string, args ...any) {
	if !powermetricsDebug {
		return
	}
	log.Printf("[powermetrics] "+format, args...)
}

func isSamplerToken(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case '_', '+', '-':
			continue
		default:
			return false
		}
	}
	return s != ""
}

type powermetricsParser struct {
	window       time.Duration
	emit         func(Envelope) error
	system       SystemSample
	haveInfo     bool
	frequencyMHz float64
}

func newPowermetricsParser(window time.Duration, emit func(Envelope) error) *powermetricsParser {
	if window <= 0 {
		window = defaultSampleWindow
	}
	return &powermetricsParser{window: window, emit: emit}
}

func (p *powermetricsParser) handleLine(raw string) error {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "--") {
		return nil
	}
	if powermetricsDebug {
		debugPowermetrics("line: %s", line)
	}

	if matches := procLineRegex.FindStringSubmatch(line); matches != nil {
		if powermetricsDebug {
			debugPowermetrics("GPU process regex matched: %v", matches)
		}
		return p.handleProcess(matches)
	} else {
		// Debug: show lines that don't match the GPU process regex
		if powermetricsDebug && strings.Contains(strings.ToLower(line), "pid") {
			debugPowermetrics("Line contains 'pid' but doesn't match regex: '%s'", line)
		}
	}

	lower := strings.ToLower(line)
	updated := false

	if strings.Contains(lower, "cpu power") && !strings.Contains(lower, "gpu") {
		if val, ok := parseTrailingValue(line, "w"); ok {
			p.system.CPUPowerWatts = val
			updated = true
		}
	}
	if strings.Contains(lower, "cpu") && !strings.Contains(lower, "gpu") && strings.Contains(lower, "frequency") {
		if val, ok := parseTrailingValue(line, "mhz"); ok {
			p.system.CPUFrequencyMHz = val
			updated = true
		}
	}
	if strings.Contains(lower, "gpu busy") {
		if val, ok := parseTrailingValue(line, "%"); ok {
			p.system.GPUBusyPercent = val
			updated = true
			debugPowermetrics("parsed GPU busy: %.2f%% from \"%s\"", val, line)
		}
	}
	if strings.Contains(lower, "gpu hw active residency") {
		if val, ok := parseTrailingValue(line, "%"); ok {
			p.system.GPUBusyPercent = val
			updated = true
			debugPowermetrics("parsed GPU hw active residency as busy: %.2f%%", val)
		}
	}
	if strings.Contains(lower, "gpu idle residency") {
		if val, ok := parseTrailingValue(line, "%"); ok {
			if p.system.GPUBusyPercent == 0 {
				p.system.GPUBusyPercent = 100 - val
				if p.system.GPUBusyPercent < 0 {
					p.system.GPUBusyPercent = 0
				}
			}
			updated = true
			debugPowermetrics("parsed GPU idle residency %.2f%% -> busy %.2f%%", val, p.system.GPUBusyPercent)
		}
	}
	if strings.Contains(lower, "ane busy") {
		if val, ok := parseTrailingValue(line, "%"); ok {
			p.system.ANEBusyPercent = val
			updated = true
		}
	}
	if strings.Contains(lower, "gpu power") {
		if val, ok := parseTrailingValue(line, "w"); ok {
			p.system.GPUPowerWatts = val
			updated = true
		}
	}
	if strings.Contains(lower, "dram") && strings.Contains(lower, "power") {
		if val, ok := parseTrailingValue(line, "w"); ok {
			p.system.DRAMPowerWatts = val
			updated = true
		}
	}
	if strings.Contains(lower, "gpu") && strings.Contains(lower, "frequency") {
		if val, ok := parseTrailingValue(line, "mhz"); ok {
			p.frequencyMHz = val
			p.system.GPUFrequencyMHz = val
			updated = true
		}
	}
	if strings.Contains(lower, "gpu") && strings.Contains(lower, "temperature") {
		if val, ok := parseTrailingValue(line, "c"); ok {
			p.system.GPUTemperatureC = val
			updated = true
		}
	}
	if strings.Contains(lower, "cpu") && strings.Contains(lower, "temperature") {
		if val, ok := parseTrailingValue(line, "c"); ok {
			p.system.CPUTemperatureC = val
			updated = true
		}
	}

	if updated {
		p.haveInfo = true
		return p.emitSystem()
	}

	return nil
}

func (p *powermetricsParser) handleProcess(matches []string) error {
	pidStr := matches[1]
	name := strings.TrimSpace(matches[2])
	valueStr := matches[3]
	unit := matches[4]
	percentStr := matches[5]

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return nil
	}
	val, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return nil
	}

	activeNs := convertToNanoseconds(val, unit)
	busy := 0.0
	if percentStr != "" {
		if parsed, err := strconv.ParseFloat(percentStr, 64); err == nil {
			busy = parsed
		}
	}
	if busy == 0 && p.window > 0 {
		busy = (float64(activeNs) / float64(p.window.Nanoseconds())) * 100
	}
	if busy > 100 {
		busy = 100
	}
	if busy < 0 {
		busy = 0
	}

	name = strings.Trim(name, "()")

	sample := GPUProcessSample{
		PID:          pid,
		Name:         name,
		BusyPercent:  busy,
		ActiveNanos:  activeNs,
		FrequencyMHz: p.frequencyMHz,
	}

	return p.emit(Envelope{Timestamp: time.Now(), Source: SourceHelper, Payload: sample})
}

func (p *powermetricsParser) emitSystem() error {
	if !p.haveInfo {
		return nil
	}
	// Populate the core information based on the detected cluster info
	sample := p.system
	return p.emit(Envelope{Timestamp: time.Now(), Source: SourceHelper, Payload: sample})
}

func parseTrailingValue(line string, suffix string) (float64, bool) {
	idx := strings.LastIndex(strings.ToLower(line), strings.ToLower(suffix))
	if idx == -1 {
		return 0, false
	}
	segment := line[:idx]
	if parenIdx := strings.Index(segment, "("); parenIdx != -1 {
		segment = segment[:parenIdx]
	}
	if colonIdx := strings.LastIndex(segment, ":"); colonIdx != -1 {
		segment = segment[colonIdx+1:]
	}
	matches := numberExtractor.FindAllString(segment, -1)
	if len(matches) == 0 {
		debugPowermetrics("parseTrailingValue failed to find number in \"%s\" for suffix %s", line, suffix)
		return 0, false
	}
	val, err := strconv.ParseFloat(matches[len(matches)-1], 64)
	if err != nil {
		debugPowermetrics("parseTrailingValue parse error in \"%s\": %v", line, err)
		return 0, false
	}
	return val, true
}

func convertToNanoseconds(value float64, unit string) uint64 {
	switch strings.ToLower(unit) {
	case "us":
		return uint64(value * 1e3)
	case "ms":
		return uint64(value * 1e6)
	case "s":
		return uint64(value * 1e9)
	default:
		return 0
	}
}
