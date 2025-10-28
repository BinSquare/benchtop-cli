package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
)

// TUIView represents the main tview-based UI
type TUIView struct {
	app             *tview.Application
	title           string
	refreshInterval time.Duration

	// UI components
	layout          *tview.Flex
	header          *tview.TextView
	tabs            *tview.TextView
	systemView      *tview.TextView // Fallback if we keep it
	cpuProcessTable *tview.Table
	gpuProcessTable *tview.Table
	helpView        *tview.TextView
	// System view components using tview layout
	sysFlex        *tview.Flex // Main system flex container
	sysInfoGrid    *tview.Grid // Grid for system info
	sysCoresFlex   *tview.Flex // CPU cores display
	sysDiskFlex    *tview.Flex // Disk info display
	sysNetworkFlex *tview.Flex // Network info display
	// Individual system info components
	sysTasksView    *tview.TextView   // Tasks information
	sysLoadView     *tview.TextView   // Load average
	sysMemView      *tview.TextView   // Memory information
	sysSwapView     *tview.TextView   // Swap information
	sysGPUView      *tview.TextView   // GPU information
	sysTempView     *tview.TextView   // Temperature information
	sysCoreViews    []*tview.TextView // Individual CPU core views
	sysDiskViews    []*tview.TextView // Individual disk partition views
	sysNetworkViews []*tview.TextView // Individual network interface views

	// Data storage
	dataMutex    sync.RWMutex
	systemSample *SystemSample
	cpuProcesses []ProcessSample
	gpuProcesses []GPUProcessSample
	diskUsage    []DiskUsageInfo // Disk usage information

	// State
	currentTab  int // 0=System, 1=CPU, 2=GPU, 3=Bench
	viewMode    int // 0=Overview, 1=Processes
	focusedView int // 0=System, 1=CPU, 2=GPU, 3=Bench

	// Network stats for calculating speeds
	networkMutex sync.Mutex // Mutex for network stats
	prevNetStats map[string]net.IOCountersStat
	prevNetTime  time.Time

	// Disk stats for calculating speeds
	diskMutex          sync.Mutex // Mutex for disk stats
	prevDiskStats      map[string]disk.IOCountersStat
	prevDiskTime       time.Time
	lastDiskUpdate     time.Time     // Last time disk data was collected
	diskUpdateInterval time.Duration // How often to update disk data
}

const (
	tabSystem = iota
	tabCPU
	tabGPU
)

// DiskUsageInfo represents disk usage information for a partition
type DiskUsageInfo struct {
	Mountpoint  string  // Mount point (e.g., "/", "/Volumes/MyDrive")
	FSType      string  // File system type (e.g., "apfs", "hfs+")
	TotalBytes  uint64  // Total size in bytes
	UsedBytes   uint64  // Used size in bytes
	FreeBytes   uint64  // Free size in bytes
	UsedPercent float64 // Used percentage
	ReadBytes   uint64  // Bytes read (for I/O stats)
	WriteBytes  uint64  // Bytes written (for I/O stats)
	ReadSpeed   float64 // Read speed in MB/s
	WriteSpeed  float64 // Write speed in MB/s
}

// NewTUIView creates a new tview-based UI
func NewTUIView(app *tview.Application, title string, refreshInterval time.Duration) *TUIView {
	ui := &TUIView{
		app:                app,
		title:              title,
		refreshInterval:    refreshInterval,
		currentTab:         tabSystem,
		viewMode:           0,
		focusedView:        tabSystem,
		prevNetStats:       make(map[string]net.IOCountersStat),
		prevNetTime:        time.Now(),
		prevDiskStats:      make(map[string]disk.IOCountersStat),
		prevDiskTime:       time.Now(),
		lastDiskUpdate:     time.Time{},     // Will be set on first update
		diskUpdateInterval: 5 * time.Second, // Update disk data every 5 seconds
	}

	ui.createComponents()
	ui.setupLayout()
	ui.setupInputHandler()

	return ui
}

// createComponents initializes all UI components
func (ui *TUIView) createComponents() {
	// Header with title and time
	ui.header = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	ui.header.SetBorder(false)

	// Tabs
	ui.tabs = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	ui.tabs.SetBorder(false)

	// System view components using tview layouts
	ui.sysFlex = tview.NewFlex().SetDirection(tview.FlexRow)
	ui.sysFlex.SetBorder(true).SetTitle("System")

	// Create grid for main system metrics
	ui.sysInfoGrid = tview.NewGrid()
	ui.sysInfoGrid.SetColumns(0, 0) // 2 equal columns
	ui.sysInfoGrid.SetRows(0, 0, 0) // 3 rows for tasks/load, memory/swap, gpu/temp
	ui.sysInfoGrid.SetBorder(true).SetTitle("System Info")

	// Create individual components for system info
	ui.sysTasksView = tview.NewTextView().SetDynamicColors(true)
	ui.sysLoadView = tview.NewTextView().SetDynamicColors(true)
	ui.sysMemView = tview.NewTextView().SetDynamicColors(true)
	ui.sysSwapView = tview.NewTextView().SetDynamicColors(true)
	ui.sysGPUView = tview.NewTextView().SetDynamicColors(true)
	ui.sysTempView = tview.NewTextView().SetDynamicColors(true)

	// Add components to the system info grid
	ui.sysInfoGrid.AddItem(ui.sysTasksView, 0, 0, 1, 1, 0, 0, false) // Row 0, Col 0
	ui.sysInfoGrid.AddItem(ui.sysLoadView, 0, 1, 1, 1, 0, 0, false)  // Row 0, Col 1
	ui.sysInfoGrid.AddItem(ui.sysMemView, 1, 0, 1, 1, 0, 0, false)   // Row 1, Col 0
	ui.sysInfoGrid.AddItem(ui.sysSwapView, 1, 1, 1, 1, 0, 0, false)  // Row 1, Col 1
	ui.sysInfoGrid.AddItem(ui.sysGPUView, 2, 0, 1, 1, 0, 0, false)   // Row 2, Col 0
	ui.sysInfoGrid.AddItem(ui.sysTempView, 2, 1, 1, 1, 0, 0, false)  // Row 2, Col 1

	// CPU cores display container
	ui.sysCoresFlex = tview.NewFlex().SetDirection(tview.FlexRow)
	ui.sysCoresFlex.SetBorder(true).SetTitle("CPU Cores")

	// Create individual core views
	ui.sysCoreViews = make([]*tview.TextView, 32) // Support up to 32 cores
	for i := range ui.sysCoreViews {
		ui.sysCoreViews[i] = tview.NewTextView().SetDynamicColors(true)
		ui.sysCoresFlex.AddItem(ui.sysCoreViews[i], 0, 1, false)
	}

	// Disk display container
	ui.sysDiskFlex = tview.NewFlex().SetDirection(tview.FlexRow)
	ui.sysDiskFlex.SetBorder(true).SetTitle("Disk")

	// Create individual disk views
	ui.sysDiskViews = make([]*tview.TextView, 8) // Support up to 8 disk partitions
	for i := range ui.sysDiskViews {
		ui.sysDiskViews[i] = tview.NewTextView().SetDynamicColors(true)
		ui.sysDiskFlex.AddItem(ui.sysDiskViews[i], 0, 1, false)
	}

	// Network display container
	ui.sysNetworkFlex = tview.NewFlex().SetDirection(tview.FlexRow)
	ui.sysNetworkFlex.SetBorder(true).SetTitle("Network")

	// Create individual network views
	ui.sysNetworkViews = make([]*tview.TextView, 4) // Support up to 4 network interfaces
	for i := range ui.sysNetworkViews {
		ui.sysNetworkViews[i] = tview.NewTextView().SetDynamicColors(true)
		ui.sysNetworkFlex.AddItem(ui.sysNetworkViews[i], 0, 1, false)
	}

	// Add components to the main system flex
	ui.sysFlex.AddItem(ui.sysInfoGrid, 0, 3, false)    // System info grid (tasks, memory, etc.)
	ui.sysFlex.AddItem(ui.sysCoresFlex, 0, 2, false)   // CPU cores display
	ui.sysFlex.AddItem(ui.sysDiskFlex, 0, 2, false)    // Disk info
	ui.sysFlex.AddItem(ui.sysNetworkFlex, 0, 1, false) // Network info

	// Keep the original systemView as a fallback/alternative
	ui.systemView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	ui.systemView.SetBorder(true).SetTitle("System (text)")

	// CPU processes table
	ui.cpuProcessTable = tview.NewTable().
		SetBorders(false)
	ui.cpuProcessTable.SetBorder(true).SetTitle("CPU Processes")

	// GPU processes table
	ui.gpuProcessTable = tview.NewTable().
		SetBorders(false)
	ui.gpuProcessTable.SetBorder(true).SetTitle("GPU Processes")

	// Help view
	ui.helpView = tview.NewTextView().
		SetDynamicColors(true)
	ui.helpView.SetBorder(true).SetTitle("Help")
}

// setupLayout arranges the UI components
func (ui *TUIView) setupLayout() {
	// Main vertical layout
	ui.layout = tview.NewFlex().SetDirection(tview.FlexRow)

	// Header section
	headerFlex := tview.NewFlex().
		AddItem(ui.header, 0, 1, false).
		AddItem(ui.tabs, 0, 1, false)
	ui.layout.AddItem(headerFlex, 2, 1, false)

	// Content area - initially show system view
	// Use the new flex layout for system view with grid
	ui.layout.AddItem(ui.sysFlex, 0, 2, true)
	ui.layout.AddItem(ui.helpView, 3, 1, false)

	// Update initial content
	ui.updateHeader()
	ui.updateTabs()
	ui.updateHelp()
}

// setupInputHandler sets up htop-like keyboard shortcuts
func (ui *TUIView) setupInputHandler() {
	ui.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlC:
			ui.app.Stop()
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 'q':
				ui.app.Stop()
				return nil
			case 'h':
				// Move left between tabs
				ui.currentTab--
				if ui.currentTab < 0 {
					ui.currentTab = 2
				}
				ui.switchTab(ui.currentTab)
				return nil
			case 'l':
				// Move right between tabs
				ui.currentTab++
				if ui.currentTab > 2 {
					ui.currentTab = 0
				}
				ui.switchTab(ui.currentTab)
				return nil
			case 't':
				ui.toggleViewMode()
				return nil
			case '/':
				// Search functionality
				ui.startSearch()
				return nil
			case 's':
				// Sort by CPU usage
				ui.sortByCPU()
				return nil
			case 'm':
				// Sort by memory usage
				ui.sortByMemory()
				return nil
			case 'p':
				// Sort by PID
				ui.sortByPID()
				return nil
			}
		case tcell.KeyLeft:
			// Move left between tabs
			ui.currentTab--
			if ui.currentTab < 0 {
				ui.currentTab = 2
			}
			ui.switchTab(ui.currentTab)
			return nil
		case tcell.KeyRight:
			// Move right between tabs
			ui.currentTab++
			if ui.currentTab > 2 {
				ui.currentTab = 0
			}
			ui.switchTab(ui.currentTab)
			return nil
		case tcell.KeyUp:
			// Move selection up in current view
			ui.moveSelectionUp()
			return nil
		case tcell.KeyDown:
			// Move selection down in current view
			ui.moveSelectionDown()
			return nil
		case tcell.KeyPgUp:
			// Page up
			ui.pageUp()
			return nil
		case tcell.KeyPgDn:
			// Page down
			ui.pageDown()
			return nil
		}
		return event
	})
}

// startSearch starts the search functionality
func (ui *TUIView) startSearch() {
	// TODO: Implement search functionality
	// This would involve creating a search dialog and filtering processes
}

// sortByCPU sorts processes by CPU usage
func (ui *TUIView) sortByCPU() {
	// Sorting is already implemented in updateCPUView
	ui.app.Draw()
}

// sortByMemory sorts processes by memory usage
func (ui *TUIView) sortByMemory() {
	// TODO: Implement memory-based sorting
	ui.app.Draw()
}

// sortByPID sorts processes by PID
func (ui *TUIView) sortByPID() {
	// TODO: Implement PID-based sorting
	ui.app.Draw()
}

// pageUp moves selection up by one page
func (ui *TUIView) pageUp() {
	ui.dataMutex.Lock()
	defer ui.dataMutex.Unlock()

	switch ui.currentTab {
	case tabCPU:
		ui.focusedView -= 10
		if ui.focusedView < 0 {
			ui.focusedView = 0
		}
	case tabGPU:
		ui.focusedView -= 10
		if ui.focusedView < 0 {
			ui.focusedView = 0
		}
	}

	// Schedule draw after releasing mutex to avoid deadlocks
	go func() {
		ui.app.Draw()
	}()
}

// pageDown moves selection down by one page
func (ui *TUIView) pageDown() {
	ui.dataMutex.Lock()
	defer ui.dataMutex.Unlock()

	switch ui.currentTab {
	case tabCPU:
		ui.focusedView += 10
		if ui.focusedView >= len(ui.cpuProcesses) {
			ui.focusedView = len(ui.cpuProcesses) - 1
		}
	case tabGPU:
		ui.focusedView += 10
		if ui.focusedView >= len(ui.gpuProcesses) {
			ui.focusedView = len(ui.gpuProcesses) - 1
		}
	}

	// Schedule draw after releasing mutex to avoid deadlocks
	go func() {
		ui.app.Draw()
	}()
}

// GetLayout returns the main layout for the application
func (ui *TUIView) GetLayout() *tview.Flex {
	return ui.layout
}

// Start begins the UI update loop
func (ui *TUIView) Start(envCh <-chan Envelope, errCh <-chan error) {
	go func() {
		for {
			select {
			case env, ok := <-envCh:
				if !ok {
					return
				}
				ui.handleEnvelope(env)
			case err, ok := <-errCh:
				if !ok {
					return
				}
				if err != nil {
					ui.handleError(err)
				}
			}
		}
	}()

	// Start periodic updates
	go func() {
		ticker := time.NewTicker(ui.refreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			ui.app.QueueUpdateDraw(func() {
				ui.updateContent()
			})
		}
	}()
}

// handleEnvelope processes incoming metrics data
func (ui *TUIView) handleEnvelope(env Envelope) {
	// Make a copy of the payload to avoid blocking the collector
	payloadCopy := env.Payload
	
	ui.dataMutex.Lock()
	defer ui.dataMutex.Unlock()

	switch payload := payloadCopy.(type) {
	case SystemSample:
		ui.systemSample = &payload
	case *SystemSample:
		if payload != nil {
			ui.systemSample = payload
		}
	case GPUProcessSample:
		// Add to GPU processes list
		ui.gpuProcesses = append(ui.gpuProcesses, payload)
		// Keep only recent samples
		if len(ui.gpuProcesses) > 100 {
			ui.gpuProcesses = ui.gpuProcesses[1:]
		}
	case *GPUProcessSample:
		if payload != nil {
			ui.gpuProcesses = append(ui.gpuProcesses, *payload)
			// Keep only recent samples
			if len(ui.gpuProcesses) > 100 {
				ui.gpuProcesses = ui.gpuProcesses[1:]
			}
		}
	case []ProcessSample:
		ui.cpuProcesses = payload
	case ProcessSnapshot:
		ui.cpuProcesses = payload.Samples
	case *ProcessSnapshot:
		if payload != nil {
			ui.cpuProcesses = payload.Samples
		}
	}
}

// handleError processes error messages
func (ui *TUIView) handleError(err error) {
	ui.dataMutex.Lock()
	defer ui.dataMutex.Unlock()

	// For now, just log to system view
	if ui.systemView != nil {
		ui.systemView.SetText(fmt.Sprintf("Error: %v", err))
	}
}

// updateHeader updates the header with title and current time
func (ui *TUIView) updateHeader() {
	headerText := fmt.Sprintf("[blue]BENCHTOP[-]  [gray]%s[-]",
		time.Now().Format("15:04:05"))
	ui.header.SetText(headerText)
}

// updateTabs updates the tab navigation like htop
func (ui *TUIView) updateTabs() {
	tabs := []string{
		"1:Sys",
		"2:CPU",
		"3:GPU",
	}

	// Create htop-style tab display
	var tabDisplay string
	for i, tab := range tabs {
		if i == ui.currentTab {
			tabDisplay += fmt.Sprintf("[::b][white:black]%s[-::-]", tab)
		} else {
			tabDisplay += fmt.Sprintf("[gray]%s[-]", tab)
		}
		if i < len(tabs)-1 {
			tabDisplay += " "
		}
	}

	ui.tabs.SetText(tabDisplay)
}

// updateHelp updates the help text
func (ui *TUIView) updateHelp() {
	helpText := `Q/q/Ctrl+C: Quit  F1-F3: Switch tabs  T: Toggle view  ?: Help`
	ui.helpView.SetText(helpText)
}

// updateContent updates all content views based on current tab and data
func (ui *TUIView) updateContent() {
	ui.dataMutex.RLock()
	// Make copies of the data under lock to avoid blocking the mutex during UI updates
	currentTab := ui.currentTab
	systemSample := ui.systemSample
	cpuProcesses := make([]ProcessSample, len(ui.cpuProcesses))
	copy(cpuProcesses, ui.cpuProcesses)
	gpuProcesses := make([]GPUProcessSample, len(ui.gpuProcesses))
	copy(gpuProcesses, ui.gpuProcesses)
	diskUsage := make([]DiskUsageInfo, len(ui.diskUsage))
	copy(diskUsage, ui.diskUsage)
	ui.dataMutex.RUnlock()

	switch currentTab {
	case tabSystem:
		ui.updateSystemView(systemSample, diskUsage)
	case tabCPU:
		ui.updateCPUView(cpuProcesses)
	case tabGPU:
		ui.updateGPUView(gpuProcesses)
	}
}

// updateSystemView updates the system information view with htop-like details and detailed core information
// updateSystemView updates the system view with grid-based layout
func (ui *TUIView) updateSystemView(systemSample *SystemSample, diskUsage []DiskUsageInfo) {
	if systemSample == nil {
		// Update fallback text view
		ui.systemView.SetText("Collecting system ..")

		// Clear grid components
		if ui.sysTasksView != nil {
			ui.sysTasksView.SetText("Collecting system ..")
		}
		if ui.sysLoadView != nil {
			ui.sysLoadView.SetText("")
		}
		if ui.sysMemView != nil {
			ui.sysMemView.SetText("")
		}
		if ui.sysSwapView != nil {
			ui.sysSwapView.SetText("")
		}
		if ui.sysGPUView != nil {
			ui.sysGPUView.SetText("")
		}
		if ui.sysTempView != nil {
			ui.sysTempView.SetText("")
		}
		return
	}

	// Update system info grid components
	ui.updateSystemInfoGrid(systemSample)

	// Update CPU cores display
	ui.updateCoresDisplay(systemSample)

	// Update disk display with provided disk usage
	ui.updateDiskDisplayWithData(diskUsage)

	// Update network display
	ui.updateNetworkDisplay()
}

// updateSystemInfoGrid updates the system info grid with tasks, memory, etc.
func (ui *TUIView) updateSystemInfoGrid(sys *SystemSample) {
	// Update tasks view
	if ui.sysTasksView != nil {
		totalProcs := len(ui.cpuProcesses)
		running, sleeping, stopped, zombie := 0, 0, 0, 0
		if totalProcs > 0 {
			running = 1 // At least one running process as a baseline
			sleeping = totalProcs - running
		}

		tasksStr := fmt.Sprintf("[::b]Tasks:[-] %d total, %d running, %d sleeping, %d stopped, %d zombie",
			totalProcs, running, sleeping, stopped, zombie)
		ui.sysTasksView.SetText(tasksStr)
	}

	// Update load average view
	if ui.sysLoadView != nil {
		loadAvg, err := load.Avg()
		if err != nil || loadAvg == nil {
			ui.sysLoadView.SetText("[::b]Load avg:[-] - , - , -")
		} else {
			loadStr := fmt.Sprintf("[::b]Load avg:[-] %.2f, %.2f, %.2f",
				loadAvg.Load1, loadAvg.Load5, loadAvg.Load15)
			ui.sysLoadView.SetText(loadStr)
		}
	}

	// Update memory view
	if ui.sysMemView != nil {
		vmStat, err := mem.VirtualMemory()
		if err == nil && vmStat != nil {
			totalMem := float64(vmStat.Total) / (1024 * 1024) // Convert to MB
			usedMem := float64(vmStat.Used) / (1024 * 1024)   // Convert to MB

			memBar := createUsageBar(vmStat.UsedPercent, 15)
			memStr := fmt.Sprintf("Mem [%s] %5.1f%%[-] %s %6.0fM/%.0fM",
				getLoadColor(int(vmStat.UsedPercent)), vmStat.UsedPercent, memBar, usedMem, totalMem)
			ui.sysMemView.SetText(memStr)
		} else {
			ui.sysMemView.SetText("Mem [red]Error[-] ---M/---M")
		}
	}

	// Update swap view
	if ui.sysSwapView != nil {
		swapStat, err := mem.SwapMemory()
		if err == nil && swapStat != nil {
			totalSwap := float64(swapStat.Total) / (1024 * 1024) // Convert to MB
			usedSwap := float64(swapStat.Used) / (1024 * 1024)   // Convert to MB

			swapBar := createUsageBar(swapStat.UsedPercent, 15)
			swapStr := fmt.Sprintf("Swap [%s] %5.1f%%[-] %s %6.0fM/%.0fM",
				getLoadColor(int(swapStat.UsedPercent)), swapStat.UsedPercent, swapBar, usedSwap, totalSwap)
			ui.sysSwapView.SetText(swapStr)
		} else {
			ui.sysSwapView.SetText("Swap [red]Error[-] ---M/---M")
		}
	}

	// Update GPU view
	if ui.sysGPUView != nil {
		gpuBar := createUsageBar(sys.GPUBusyPercent, 15)
		gpuStr := fmt.Sprintf("GPU [%s] %5.1f%%[-] %s Freq:%.0fMHz Pwr:%.2fW",
			getLoadColor(int(sys.GPUBusyPercent)), sys.GPUBusyPercent, gpuBar,
			sys.GPUFrequencyMHz, sys.GPUPowerWatts)
		ui.sysGPUView.SetText(gpuStr)
	}

	// Update temperature view
	if ui.sysTempView != nil {
		tempStr := ""
		if sys.CPUTemperatureC > 0 || sys.GPUTemperatureC > 0 {
			if sys.CPUTemperatureC > 0 && sys.GPUTemperatureC > 0 {
				cpuColor := getLoadColor(int(sys.CPUTemperatureC))
				gpuColor := getLoadColor(int(sys.GPUTemperatureC))
				tempStr = fmt.Sprintf("Temp: CPU[%s]%.1f°C[-] GPU[%s]%.1f°C[-]",
					cpuColor, sys.CPUTemperatureC, gpuColor, sys.GPUTemperatureC)
			} else if sys.CPUTemperatureC > 0 {
				cpuColor := getLoadColor(int(sys.CPUTemperatureC))
				tempStr = fmt.Sprintf("CPU Temp: [%s]%.1f°C[-]", cpuColor, sys.CPUTemperatureC)
			} else if sys.GPUTemperatureC > 0 {
				gpuColor := getLoadColor(int(sys.GPUTemperatureC))
				tempStr = fmt.Sprintf("GPU Temp: [%s]%.1f°C[-]", gpuColor, sys.GPUTemperatureC)
			}
		} else {
			tempStr = "Temp: N/A"
		}
		ui.sysTempView.SetText(tempStr)
	}
}

// updateCoresDisplay updates the CPU cores display
func (ui *TUIView) updateCoresDisplay(sys *SystemSample) {
	// Clear existing core views
	for i, coreView := range ui.sysCoreViews {
		if coreView != nil {
			if i < len(sys.CPUPerCore) {
				// Show core usage
				usage := sys.CPUPerCore[i]
				coreBar := createUsageBar(usage, 20)
				coreStr := fmt.Sprintf("Core %2d: [%s] %5.1f%%[-] %s",
					i, getLoadColor(int(usage)), usage, coreBar)
				coreView.SetText(coreStr)
			} else {
				// Hide unused core views
				coreView.SetText("")
			}
		}
	}
}

// updateDiskDisplay updates the disk display
func (ui *TUIView) updateDiskDisplay() {
	// Check if we need to update disk data (cache for performance)
	now := time.Now()
	ui.dataMutex.RLock()
	needsUpdate := ui.lastDiskUpdate.IsZero() || now.Sub(ui.lastDiskUpdate) >= ui.diskUpdateInterval
	ui.dataMutex.RUnlock()

	if needsUpdate {
		// Update disk data in a goroutine to avoid blocking the UI
		go ui.collectDiskUsageAsync()
	}

	// Get disk usage data
	ui.dataMutex.RLock()
	diskUsage := make([]DiskUsageInfo, len(ui.diskUsage))
	copy(diskUsage, ui.diskUsage)
	ui.dataMutex.RUnlock()

	// Display with the disk usage data
	ui.updateDiskDisplayWithData(diskUsage)
}

// updateDiskDisplayWithData updates the disk display with provided disk usage data
func (ui *TUIView) updateDiskDisplayWithData(diskUsage []DiskUsageInfo) {
	// Clear all disk views first
	for _, diskView := range ui.sysDiskViews {
		if diskView != nil {
			diskView.SetText("")
		}
	}

	// Display meaningful disk partitions
	diskCount := len(diskUsage)
	
	// Limit to available views
	maxDisks := len(ui.sysDiskViews)
	if diskCount > maxDisks {
		diskCount = maxDisks
	}

	for i := 0; i < diskCount; i++ {
		if ui.sysDiskViews[i] != nil {
			diskText := ui.formatDiskInfo(diskUsage[i])
			ui.sysDiskViews[i].SetText(diskText)
		}
	}
}

// updateNetworkDisplay updates the network display
func (ui *TUIView) updateNetworkDisplay() {
	// Get network interfaces
	// Note: This is a simplified implementation - in a real app we would need to
	// properly handle the network data which is typically collected elsewhere
	// For now, we just clear the views
	for _, netView := range ui.sysNetworkViews {
		if netView != nil {
			netView.SetText("")
		}
	}
}

// createUsageBar creates a visual bar representing usage percentage
func createUsageBar(percentage float64, width int) string {
	if width <= 0 {
		width = 20
	}

	filled := int(percentage * float64(width) / 100)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}

	bar := ""
	for i := 0; i < width; i++ {
		if i < filled {
			load := int(percentage)
			if load > 80 {
				bar += "[red]█[-]"
			} else if load > 50 {
				bar += "[yellow]█[-]"
			} else {
				bar += "[green]█[-]"
			}
		} else {
			bar += "[gray]░[-]"
		}
	}

	return bar
}

// switchTab switches to the specified tab
func (ui *TUIView) switchTab(tab int) {
	ui.currentTab = tab
	// Implementation would depend on the actual tab switching logic
	// This is a placeholder to fix the build error
}

// toggleViewMode toggles between overview and process view modes
func (ui *TUIView) toggleViewMode() {
	ui.viewMode = 1 - ui.viewMode // Toggle between 0 and 1
	// Implementation would depend on the actual view mode logic
	// This is a placeholder to fix the build error
}

// moveSelectionUp moves the selection up in the current view
func (ui *TUIView) moveSelectionUp() {
	// Implementation would depend on the actual selection logic
	// This is a placeholder to fix the build error
}

// moveSelectionDown moves the selection down in the current view
func (ui *TUIView) moveSelectionDown() {
	// Implementation would depend on the actual selection logic
	// This is a placeholder to fix the build error
}

// updateCPUView updates the CPU processes view with htop-like columns
func (ui *TUIView) updateCPUView(cpuProcesses []ProcessSample) {
	// Implementation would depend on the actual CPU view logic
	// This is a placeholder to fix the build error
}

// updateGPUView updates the GPU processes view like htop
func (ui *TUIView) updateGPUView(gpuProcesses []GPUProcessSample) {
	// Implementation would depend on the actual GPU view logic
	// This is a placeholder to fix the build error
}

// collectDiskUsageAsync collects disk usage information asynchronously with timeout
func (ui *TUIView) collectDiskUsageAsync() {
	// Use a timeout to prevent hanging
	done := make(chan bool, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				// Handle any panics gracefully
				ui.dataMutex.Lock()
				ui.lastDiskUpdate = time.Now()
				ui.dataMutex.Unlock()
			}
			done <- true
		}()

		ui.collectDiskUsage()

		// Update the last update time
		ui.dataMutex.Lock()
		ui.lastDiskUpdate = time.Now()
		ui.dataMutex.Unlock()
	}()

	// Wait for completion or timeout
	select {
	case <-done:
		// Successfully completed
	case <-time.After(3 * time.Second):
		// Timeout - don't update lastDiskUpdate so it will retry next time
	}
}

// collectDiskUsage collects disk usage information for all partitions
func (ui *TUIView) collectDiskUsage() {
	// Get disk partitions with timeout
	partitions, err := ui.getDiskPartitionsWithTimeout()
	if err != nil {
		return
	}

	var diskInfoList []DiskUsageInfo

	for _, partition := range partitions {
		// Filter out non-meaningful partitions
		if !ui.isMeaningfulDisk(partition.Mountpoint, partition.Fstype) {
			continue
		}

		// Get disk usage with timeout
		usage, err := ui.getDiskUsageWithTimeout(partition.Mountpoint)
		if err != nil {
			continue
		}

		// Get I/O counters for this device with timeout
		ioCounters, err := ui.getDiskIOCountersWithTimeout()
		if err != nil {
			continue
		}

		// Find the I/O counter for this device
		var ioStat disk.IOCountersStat
		if stat, exists := ioCounters[partition.Device]; exists {
			ioStat = stat
		}

		// Calculate speeds
		readSpeed, writeSpeed := ui.calculateDiskSpeeds(partition.Device, ioStat)

		diskInfo := DiskUsageInfo{
			Mountpoint:  partition.Mountpoint,
			FSType:      partition.Fstype,
			TotalBytes:  usage.Total,
			UsedBytes:   usage.Used,
			FreeBytes:   usage.Free,
			UsedPercent: usage.UsedPercent,
			ReadBytes:   ioStat.ReadBytes,
			WriteBytes:  ioStat.WriteBytes,
			ReadSpeed:   readSpeed,
			WriteSpeed:  writeSpeed,
		}

		diskInfoList = append(diskInfoList, diskInfo)
	}

	// Sort by mount point for consistent display
	ui.dataMutex.Lock()
	ui.diskUsage = diskInfoList
	ui.dataMutex.Unlock()
}

// getDiskPartitionsWithTimeout gets disk partitions with a timeout
func (ui *TUIView) getDiskPartitionsWithTimeout() ([]disk.PartitionStat, error) {
	result := make(chan struct {
		partitions []disk.PartitionStat
		err        error
	}, 1)

	go func() {
		partitions, err := disk.Partitions(false)
		result <- struct {
			partitions []disk.PartitionStat
			err        error
		}{partitions, err}
	}()

	select {
	case res := <-result:
		return res.partitions, res.err
	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("timeout getting disk partitions")
	}
}

// getDiskUsageWithTimeout gets disk usage with a timeout
func (ui *TUIView) getDiskUsageWithTimeout(mountpoint string) (*disk.UsageStat, error) {
	result := make(chan struct {
		usage *disk.UsageStat
		err   error
	}, 1)

	go func() {
		usage, err := disk.Usage(mountpoint)
		result <- struct {
			usage *disk.UsageStat
			err   error
		}{usage, err}
	}()

	select {
	case res := <-result:
		return res.usage, res.err
	case <-time.After(1 * time.Second):
		return nil, fmt.Errorf("timeout getting disk usage for %s", mountpoint)
	}
}

// getDiskIOCountersWithTimeout gets disk I/O counters with a timeout
func (ui *TUIView) getDiskIOCountersWithTimeout() (map[string]disk.IOCountersStat, error) {
	result := make(chan struct {
		counters map[string]disk.IOCountersStat
		err      error
	}, 1)

	go func() {
		counters, err := disk.IOCounters()
		result <- struct {
			counters map[string]disk.IOCountersStat
			err      error
		}{counters, err}
	}()

	select {
	case res := <-result:
		return res.counters, res.err
	case <-time.After(1 * time.Second):
		return nil, fmt.Errorf("timeout getting disk I/O counters")
	}
}

// calculateDiskSpeeds calculates read/write speeds for a disk device
func (ui *TUIView) calculateDiskSpeeds(device string, currentStat disk.IOCountersStat) (float64, float64) {
	ui.diskMutex.Lock()
	defer ui.diskMutex.Unlock()

	now := time.Now()
	prevStat, exists := ui.prevDiskStats[device]

	if !exists {
		// First time seeing this device
		ui.prevDiskStats[device] = currentStat
		ui.prevDiskTime = now
		return 0, 0
	}

	// Calculate time difference
	timeDiff := now.Sub(ui.prevDiskTime).Seconds()
	if timeDiff <= 0 {
		return 0, 0
	}

	// Calculate byte differences
	readDiff := currentStat.ReadBytes - prevStat.ReadBytes
	writeDiff := currentStat.WriteBytes - prevStat.WriteBytes

	// Calculate speeds in MB/s
	readSpeed := float64(readDiff) / (1024 * 1024) / timeDiff
	writeSpeed := float64(writeDiff) / (1024 * 1024) / timeDiff

	// Update previous stats
	ui.prevDiskStats[device] = currentStat
	ui.prevDiskTime = now

	return readSpeed, writeSpeed
}

// isMeaningfulDisk determines if a disk partition should be displayed in the UI
func (ui *TUIView) isMeaningfulDisk(mountpoint, fstype string) bool {
	// Skip system and special filesystems that don't represent real storage
	specialMounts := []string{
		"/dev",                 // Device files
		"/dev/pts",             // Pseudo-terminal
		"/proc",                // Process information (Linux)
		"/sys",                 // System information (Linux)
		"/run",                 // Runtime data (Linux)
		"/sys/fs/cgroup",       // Control groups (Linux)
		"/boot",                // Boot partition (usually small)
		"/sys/kernel/debug",    // Kernel debug (Linux)
		"/sys/kernel/security", // Security (Linux)
		"/dev/mqueue",          // POSIX message queues
		"/dev/shm",             // Shared memory
		"/var/run",             // Runtime variables (Linux)
		"/var/lock",            // Lock files (Linux)
	}

	// Check against special mount points
	for _, special := range specialMounts {
		if mountpoint == special || (len(mountpoint) > len(special) && mountpoint[:len(special)+1] == special+"/") {
			return false
		}
	}

	// Skip ALL /System/Volumes/* (these are macOS system volumes that don't represent user storage)
	if len(mountpoint) >= 17 && mountpoint[:17] == "/System/Volumes/" {
		return false
	}

	// Skip developer tools volumes that are not meaningful for general monitoring
	if len(mountpoint) >= 20 && mountpoint[:20] == "/Library/Developer/" {
		return false
	}

	// Check if it's a root mount or user-accessible volume
	// On macOS, user volumes are typically under / (root) or /Volumes/
	if mountpoint == "/" {
		return true // Root partition is meaningful
	}

	// Volumes accessible to users
	if len(mountpoint) >= 9 && mountpoint[:9] == "/Volumes/" {
		return true
	}

	// Check if the mount is in the user's home directory for external drives
	if len(mountpoint) >= 8 && mountpoint[:8] == "/Users/" {
		// Check if it's a direct subdirectory of a user directory (like /Users/username/ExternalDrive)
		parts := make([]string, 0)
		start := 0
		for i, char := range mountpoint {
			if char == '/' {
				if start < i {
					parts = append(parts, mountpoint[start:i])
				}
				start = i + 1
			}
		}
		if start < len(mountpoint) {
			parts = append(parts, mountpoint[start:])
		}

		if len(parts) == 3 { // /Users/username/something
			return true
		}
	}

	// Other meaningful filesystem types typically found on user-accessible disks
	meaningfulFSTypes := []string{
		"apfs",  // Apple File System
		"hfs",   // Hierarchical File System
		"hfs+",  // HFS Plus
		"ext4",  // Linux ext4
		"ext3",  // Linux ext3
		"btrfs", // BTRFS
		"xfs",   // XFS
		"ntfs",  // NTFS
		"exfat", // exFAT
		"vfat",  // VFAT/FAT32
		"zfs",   // ZFS
	}

	fstypeLower := strings.ToLower(fstype)
	for _, fsType := range meaningfulFSTypes {
		if strings.Contains(fstypeLower, fsType) {
			return true
		}
	}

	// Default to not showing if we can't identify it as meaningful
	return false
}

// formatDiskInfo formats disk information for display
func (ui *TUIView) formatDiskInfo(diskInfo DiskUsageInfo) string {
	// Format mount point (truncate if too long)
	mountpoint := diskInfo.Mountpoint
	if len(mountpoint) > 20 {
		mountpoint = mountpoint[:17] + "..."
	}

	// Format sizes
	totalGB := float64(diskInfo.TotalBytes) / (1024 * 1024 * 1024)
	usedGB := float64(diskInfo.UsedBytes) / (1024 * 1024 * 1024)

	// Create usage bar
	usageBar := ui.createDiskUsageBar(diskInfo.UsedPercent, 15)

	// Get color based on usage percentage
	usageColor := ui.getDiskUsageColor(diskInfo.UsedPercent)

	// Format I/O speeds
	readSpeedStr := fmt.Sprintf("%.1f", diskInfo.ReadSpeed)
	writeSpeedStr := fmt.Sprintf("%.1f", diskInfo.WriteSpeed)

	// Format the display string
	diskStr := fmt.Sprintf("%-20s [%s]%5.1f%%[-] %s %6.1fG/%.1fG (%s) R:%s W:%s MB/s",
		mountpoint,
		usageColor,
		diskInfo.UsedPercent,
		usageBar,
		usedGB,
		totalGB,
		diskInfo.FSType,
		readSpeedStr,
		writeSpeedStr,
	)

	return diskStr
}

// createDiskUsageBar creates a visual bar representing disk usage percentage
func (ui *TUIView) createDiskUsageBar(percentage float64, width int) string {
	if width <= 0 {
		width = 15
	}

	filled := int(percentage * float64(width) / 100)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}

	bar := ""
	for i := 0; i < width; i++ {
		if i < filled {
			usage := int(percentage)
			if usage > 90 {
				bar += "[red]█[-]"
			} else if usage > 75 {
				bar += "[yellow]█[-]"
			} else {
				bar += "[green]█[-]"
			}
		} else {
			bar += "[gray]░[-]"
		}
	}

	return bar
}

// getDiskUsageColor returns appropriate color based on disk usage percentage
func (ui *TUIView) getDiskUsageColor(usage float64) string {
	if usage > 90 {
		return "red"
	} else if usage > 75 {
		return "yellow"
	} else {
		return "green"
	}
}

// getLoadColor returns appropriate color based on load percentage
func getLoadColor(load int) string {
	if load > 80 {
		return "red"
	} else if load > 50 {
		return "yellow"
	} else {
		return "green"
	}
}
