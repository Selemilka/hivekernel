package daemons

import (
	"context"
	"log"
	"runtime"
	"time"

	"github.com/selemilka/hivekernel/internal/ipc"
	"github.com/selemilka/hivekernel/internal/process"
)

// MaidConfig holds configuration for the maid daemon.
type MaidConfig struct {
	CheckInterval   time.Duration
	MemoryThreshold float64 // fraction (0.0-1.0), e.g. 0.90 = 90%
	VPS             string
}

// DefaultMaidConfig returns sensible defaults.
func DefaultMaidConfig(vps string) MaidConfig {
	return MaidConfig{
		CheckInterval:   30 * time.Second,
		MemoryThreshold: 0.90,
		VPS:             vps,
	}
}

// Maid is a per-VPS daemon that monitors system health.
// It detects zombies, monitors resources, and reports to its parent (queen).
type Maid struct {
	config   MaidConfig
	registry *process.Registry
	signals  *process.SignalRouter
	outbox   *ipc.PriorityQueue // messages to parent (queen)
	pid      process.PID        // maid's own PID
}

// NewMaid creates a new maid daemon.
func NewMaid(
	config MaidConfig,
	registry *process.Registry,
	signals *process.SignalRouter,
	outbox *ipc.PriorityQueue,
	pid process.PID,
) *Maid {
	return &Maid{
		config:   config,
		registry: registry,
		signals:  signals,
		outbox:   outbox,
		pid:      pid,
	}
}

// Run starts the maid's monitoring loop. Blocks until ctx is cancelled.
func (m *Maid) Run(ctx context.Context) {
	log.Printf("[maid@%s] starting health monitoring (interval=%s)",
		m.config.VPS, m.config.CheckInterval)

	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[maid@%s] shutting down", m.config.VPS)
			return
		case <-ticker.C:
			m.checkHealth()
		}
	}
}

func (m *Maid) checkHealth() {
	m.checkMemory()
	m.checkZombies()
	m.checkLocalProcesses()
}

// checkMemory reports if system memory usage is above threshold.
func (m *Maid) checkMemory() {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// Report Go process memory as a basic check.
	// On a real VPS, we'd read /proc/meminfo or use cgroups.
	allocMB := float64(memStats.Alloc) / 1024 / 1024
	sysMB := float64(memStats.Sys) / 1024 / 1024

	// For Phase 1, just log. Real implementation would compare against VPS total RAM.
	if allocMB > 100 { // simple threshold for now
		log.Printf("[maid@%s] WARNING: high memory usage: alloc=%.1f MB, sys=%.1f MB",
			m.config.VPS, allocMB, sysMB)
		m.report("memory_high",
			ipc.PriorityHigh,
			[]byte("high memory usage detected"),
		)
	}
}

// checkZombies finds zombie processes on this VPS and reports them.
func (m *Maid) checkZombies() {
	var zombies []*process.Process
	for _, p := range m.registry.List() {
		if p.VPS == m.config.VPS && p.State == process.StateZombie {
			zombies = append(zombies, p)
		}
	}

	if len(zombies) > 0 {
		log.Printf("[maid@%s] found %d zombie processes", m.config.VPS, len(zombies))
		for _, z := range zombies {
			log.Printf("[maid@%s]   zombie: PID %d (%s) since %s",
				m.config.VPS, z.PID, z.Name, z.UpdatedAt.Format(time.TimeOnly))
		}
		m.report("zombies_detected",
			ipc.PriorityNormal,
			[]byte("zombie processes detected"),
		)
	}
}

// checkLocalProcesses verifies all processes assigned to this VPS are healthy.
func (m *Maid) checkLocalProcesses() {
	var blocked int
	var idle int
	var running int

	for _, p := range m.registry.List() {
		if p.VPS != m.config.VPS || p.PID == m.pid {
			continue
		}
		switch p.State {
		case process.StateBlocked:
			blocked++
		case process.StateIdle:
			idle++
		case process.StateRunning:
			running++
		}
	}

	log.Printf("[maid@%s] health: running=%d idle=%d blocked=%d",
		m.config.VPS, running, idle, blocked)
}

// report sends a message to the parent (queen) via the outbox.
func (m *Maid) report(msgType string, priority int, payload []byte) {
	proc, err := m.registry.Get(m.pid)
	if err != nil {
		return
	}

	m.outbox.Push(&ipc.Message{
		FromPID:  m.pid,
		FromName: proc.Name,
		ToPID:    proc.PPID, // queen
		Type:     msgType,
		Priority: priority,
		Payload:  payload,
	})
}
