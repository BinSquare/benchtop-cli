package main

import "time"

// Envelope wraps any sample emitted by the helper, collectors, benches, or agents.
type Envelope struct {
	Timestamp time.Time
	Source    Source
	Payload   any
}

// Source indicates the origin of a metric sample for routing/labeling.
type Source string

const (
	SourceHelper  Source = "helper"
	SourceProcess Source = "process"
	SourceSystem  Source = "system"
)

// GPUProcessSample represents telemetry for a single process on the GPU.
type GPUProcessSample struct {
	PID          int
	Name         string
	BusyPercent  float64
	ActiveNanos  uint64
	FrequencyMHz float64
	PowerWatts   float64
	TemperatureC float64
}

// CoreType represents the type of CPU core (performance or efficiency)
type CoreType string

const (
	CoreTypePerformance CoreType = "P"
	CoreTypeEfficiency  CoreType = "E"
)

// CoreInfo represents information about a single CPU core
type CoreInfo struct {
	ID       int       // Core ID
	Type     CoreType  // Core type (Performance or Efficiency)
	Usage    float64   // Core usage percentage
	FrequencyMHz float64 // Core frequency
}

// ClusterInfo represents information about a CPU cluster (Performance or Efficiency)
type ClusterInfo struct {
	Name          string  // Cluster name (e.g., "E-Cluster", "P0-Cluster", "P1-Cluster")
	Type          CoreType // Cluster type (Performance or Efficiency)
	OnlinePercent float64 // Cluster online percentage
	HWActiveFreq  float64 // Hardware active frequency
	PowerWatts    float64 // Power consumption for this cluster
	MaxFreqMHz    float64 // Maximum frequency reached
	AvgFreqMHz    float64 // Average frequency
}

// SystemSample holds host-wide metrics gathered from powermetrics and psutil.
type SystemSample struct {
	CPUTotalPercent float64
	CPUPerCore      []float64
	Cores           []CoreInfo    // Individual core information
	Clusters        []ClusterInfo // Cluster-specific information
	CPUFrequencyMHz float64
	CPUPowerWatts   float64
	GPUBusyPercent  float64
	ANEBusyPercent  float64
	DRAMPowerWatts  float64
	GPUPowerWatts   float64
	GPUFrequencyMHz float64
	GPUTemperatureC float64
	CPUTemperatureC float64
}



// ProcessSample represents CPU-oriented metrics for a single process.
type ProcessSample struct {
	PID            int
	Name           string
	ExecutablePath string      // Path to the executable
	CPUPercent     float64
	RSSBytes       uint64
	VirtualBytes   uint64
	ThreadCount    int
	ReadBytes      uint64
	WriteBytes     uint64
	CoreType       CoreType // Performance (P) or Efficiency (E) core type
	// Note: CPU frequency, power, and temperature are system/core metrics, not per-process
}

// ProcessSnapshot carries a batch of process metrics sampled at the same time.
type ProcessSnapshot struct {
	Samples []ProcessSample
}

// ErrorSample conveys collector errors to the UI.
type ErrorSample struct {
	Component string
	Message   string
}
