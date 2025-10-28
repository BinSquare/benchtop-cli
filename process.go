package main

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	gprocess "github.com/shirou/gopsutil/v3/process"
)

const (
	defaultProcessInterval = time.Second
	defaultProcessLimit    = 12
)

// ProcessConfig configures unprivileged process sampling.
type ProcessConfig struct {
	SampleInterval time.Duration
	ProcessLimit   int
}

type processStream struct {
	config           ProcessConfig
	prevSystemTimes  []cpu.TimesStat
	prevCoreTimes    []cpu.TimesStat
	prevProcessTimes map[int32]procTimes
}

type procTimes struct {
	total     float64
	timestamp time.Time
}

// StreamProcesses returns a Stream that polls per-process CPU/memory 
func StreamProcesses(cfg ProcessConfig) Stream {
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = defaultProcessInterval
	}
	if cfg.ProcessLimit <= 0 {
		cfg.ProcessLimit = defaultProcessLimit
	}
	return &processStream{
		config:           cfg,
		prevProcessTimes: make(map[int32]procTimes),
	}
}

func (p *processStream) Start(ctx context.Context) (<-chan Envelope, error) {
	out := make(chan Envelope, 128)
	go p.run(ctx, out)
	return out, nil
}

func (p *processStream) run(ctx context.Context, out chan<- Envelope) {
	defer close(out)

	ticker := time.NewTicker(p.config.SampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ts := <-ticker.C:
			if err := p.emitSystem(ctx, out, ts); err != nil && !errors.Is(err, context.Canceled) {
				// keep running despite transient errors
			}
			if err := p.emitProcesses(ctx, out, ts); err != nil && !errors.Is(err, context.Canceled) {
				// continue attempting future samples
			}
		}
	}
}

func (p *processStream) emitSystem(ctx context.Context, out chan<- Envelope, ts time.Time) error {
	totalTimes, err := cpu.TimesWithContext(ctx, false)
	if err != nil {
		return err
	}
	if len(totalTimes) == 0 {
		return nil
	}

	coreTimes, coreErr := cpu.TimesWithContext(ctx, true)
	if coreErr != nil {
		coreTimes = nil
	}

	if p.prevSystemTimes == nil || len(p.prevSystemTimes) == 0 {
		p.prevSystemTimes = totalTimes
		if len(coreTimes) > 0 {
			p.prevCoreTimes = coreTimes
		}
		// Emit an initial zero sample so the UI shows something immediately.
		initial := SystemSample{CPUTotalPercent: 0}
		if len(coreTimes) > 0 {
			initial.CPUPerCore = make([]float64, len(coreTimes))
		}
		if !sendEnvelope(ctx, out, Envelope{Timestamp: ts, Source: SourceSystem, Payload: initial}) {
			return context.Canceled
		}
		return nil
	}

	curr := totalTimes[0]
	prev := p.prevSystemTimes[0]
	busyDelta, totalDelta := cpuBusyAndTotal(curr, prev)

	if totalDelta <= 0 {
		p.prevSystemTimes = totalTimes
		if len(coreTimes) > 0 {
			p.prevCoreTimes = coreTimes
		}
		return nil
	}

	cpuPercent := clampPercent((busyDelta / totalDelta) * 100)

	var perCore []float64
	if len(coreTimes) > 0 {
		if len(p.prevCoreTimes) != len(coreTimes) || len(p.prevCoreTimes) == 0 {
			perCore = make([]float64, len(coreTimes))
		} else {
			perCore = make([]float64, len(coreTimes))
			for i := range coreTimes {
				busy, total := cpuBusyAndTotal(coreTimes[i], p.prevCoreTimes[i])
				if total > 0 {
					perCore[i] = clampPercent((busy / total) * 100)
				}
			}
		}
	}

	sample := SystemSample{
		CPUTotalPercent: cpuPercent,
		CPUPerCore:      perCore,
		// Note: Cores field is not populated here since gopsutil doesn't provide
		// detailed core type information. This field will be populated by powermetrics
		// when available, which has access to actual hardware topology.
	}
	if !sendEnvelope(ctx, out, Envelope{Timestamp: ts, Source: SourceSystem, Payload: sample}) {
		return context.Canceled
	}

	p.prevSystemTimes = totalTimes
	if len(coreTimes) > 0 {
		p.prevCoreTimes = coreTimes
	}
	return nil
}

func (p *processStream) emitProcesses(ctx context.Context, out chan<- Envelope, ts time.Time) error {
	procs, err := gprocess.ProcessesWithContext(ctx)
	if err != nil {
		return err
	}

	now := ts
	samples := make([]ProcessSample, 0, len(procs))
	seen := make(map[int32]struct{}, len(procs))

	for _, proc := range procs {
		pid := proc.Pid
		seen[pid] = struct{}{}

		name, err := proc.NameWithContext(ctx)
		if err != nil || name == "" {
			continue
		}

		timesStat, err := proc.TimesWithContext(ctx)
		if err != nil {
			continue
		}
		total := timesStat.User + timesStat.System

		prev, ok := p.prevProcessTimes[pid]
		p.prevProcessTimes[pid] = procTimes{total: total, timestamp: now}
		if !ok {
			continue // need a baseline to compute CPU percent
		}

		elapsed := now.Sub(prev.timestamp).Seconds()
		if elapsed <= 0 {
			continue
		}

		cpuPercent := (total - prev.total) / elapsed * 100
		if cpuPercent < 0 {
			continue
		}

		memInfo, err := proc.MemoryInfoWithContext(ctx)
		if err != nil || memInfo == nil {
			continue
		}

		threads, err := proc.NumThreadsWithContext(ctx)
		if err != nil {
			threads = 0
		}

		var readBytes, writeBytes uint64
		if ioCounters, err := proc.IOCountersWithContext(ctx); err == nil && ioCounters != nil {
			readBytes = ioCounters.ReadBytes
			writeBytes = ioCounters.WriteBytes
		}

		// Get executable path
		execPath, err := proc.ExeWithContext(ctx)
		if err != nil {
			execPath = "" // If we can't get the path, leave it empty
		}
		
		// For process-level core type, we'll use a heuristic approach since 
		// gopsutil doesn't provide actual core assignment information
		// In a real Apple Silicon implementation, we would need access to
		// Apple-specific APIs for accurate per-process core detection
		coreType := determineCoreType(name, cpuPercent, int(threads))
		
		sample := ProcessSample{
			PID:            int(pid),
			Name:           name,
			ExecutablePath: execPath,
			CPUPercent:     cpuPercent,
			RSSBytes:       memInfo.RSS,
			VirtualBytes:   memInfo.VMS,
			ThreadCount:    int(threads),
			ReadBytes:      readBytes,
			WriteBytes:     writeBytes,
			CoreType:       coreType,
		}
		samples = append(samples, sample)
	}

	// Trim cached process times for processes that vanished.
	for pid := range p.prevProcessTimes {
		if _, ok := seen[pid]; !ok {
			delete(p.prevProcessTimes, pid)
		}
	}

	if len(samples) == 0 {
		return nil
	}

	sort.Slice(samples, func(i, j int) bool {
		return samples[i].CPUPercent > samples[j].CPUPercent
	})

	if len(samples) > p.config.ProcessLimit {
		samples = samples[:p.config.ProcessLimit]
	}

	batch := ProcessSnapshot{Samples: samples}
	if !sendEnvelope(ctx, out, Envelope{Timestamp: ts, Source: SourceProcess, Payload: batch}) {
		return context.Canceled
	}
	return nil
}

func cpuBusyAndTotal(curr, prev cpu.TimesStat) (busy float64, total float64) {
	total = curr.Total() - prev.Total()
	idle := (curr.Idle - prev.Idle) + (curr.Iowait - prev.Iowait)
	if idle < 0 {
		idle = 0
	}
	busy = total - idle
	if busy < 0 {
		busy = 0
	}
	if total < 0 {
		total = 0
	}
	return busy, total
}

// determineCoreType attempts to determine if a process is likely running on 
// performance or efficiency cores based on heuristics
func determineCoreType(name string, cpuPercent float64, threadCount int) CoreType {
	// This is a heuristic-based approach to determine likely core type
	// In a real implementation, we would need to use Apple-specific APIs
	// to determine actual core assignment
	
	// Processes with high CPU usage are more likely on performance cores
	if cpuPercent > 50 {
		return CoreTypePerformance
	}
	
	// System processes are often on performance cores
	systemProcessNames := []string{
		"kernel_task", "launchd", "WindowServer", "Finder", 
		"Dock", "Spotlight", "mds_stores", "mds", "syslogd",
	}
	
	for _, sysName := range systemProcessNames {
		if name == sysName {
			return CoreTypePerformance
		}
	}
	
	// Processes with many threads might be on performance cores
	if threadCount > 8 {
		return CoreTypePerformance
	}
	
	// Default to efficiency for other processes
	return CoreTypeEfficiency
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
