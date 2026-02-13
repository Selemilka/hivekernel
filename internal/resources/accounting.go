package resources

import (
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// UsageRecord represents a single token consumption event.
type UsageRecord struct {
	PID       process.PID
	User      string
	VPS       string
	Tier      ModelTier
	Tokens    uint64
	Timestamp time.Time
}

// UsageSummary aggregates token consumption.
type UsageSummary struct {
	TotalTokens uint64
	ByTier      map[ModelTier]uint64
}

// Accountant tracks token usage across the system.
type Accountant struct {
	mu       sync.RWMutex
	records  []UsageRecord
	byUser   map[string]map[ModelTier]uint64
	byVPS    map[string]map[ModelTier]uint64
	byPID    map[process.PID]map[ModelTier]uint64
	registry *process.Registry
}

// NewAccountant creates an accountant backed by the process registry.
func NewAccountant(registry *process.Registry) *Accountant {
	return &Accountant{
		byUser:   make(map[string]map[ModelTier]uint64),
		byVPS:    make(map[string]map[ModelTier]uint64),
		byPID:    make(map[process.PID]map[ModelTier]uint64),
		registry: registry,
	}
}

// Record logs a token consumption event and updates aggregates.
func (a *Accountant) Record(pid process.PID, tier ModelTier, tokens uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	proc, err := a.registry.Get(pid)
	user := "unknown"
	vps := "unknown"
	if err == nil {
		user = proc.User
		vps = proc.VPS
	}

	rec := UsageRecord{
		PID:       pid,
		User:      user,
		VPS:       vps,
		Tier:      tier,
		Tokens:    tokens,
		Timestamp: time.Now(),
	}
	a.records = append(a.records, rec)

	// Update aggregates.
	a.addTo(a.byUser, user, tier, tokens)
	a.addTo(a.byVPS, vps, tier, tokens)

	if a.byPID[pid] == nil {
		a.byPID[pid] = make(map[ModelTier]uint64)
	}
	a.byPID[pid][tier] += tokens
}

func (a *Accountant) addTo(m map[string]map[ModelTier]uint64, key string, tier ModelTier, tokens uint64) {
	if m[key] == nil {
		m[key] = make(map[ModelTier]uint64)
	}
	m[key][tier] += tokens
}

// UserUsage returns aggregated usage for a user.
func (a *Accountant) UserUsage(user string) UsageSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.summarize(a.byUser[user])
}

// VPSUsage returns aggregated usage for a VPS.
func (a *Accountant) VPSUsage(vps string) UsageSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.summarize(a.byVPS[vps])
}

// ProcessUsage returns aggregated usage for a single process.
func (a *Accountant) ProcessUsage(pid process.PID) UsageSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.summarize(a.byPID[pid])
}

// TotalUsage returns total system-wide usage.
func (a *Accountant) TotalUsage() UsageSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()

	total := make(map[ModelTier]uint64)
	for _, tierMap := range a.byPID {
		for tier, tokens := range tierMap {
			total[tier] += tokens
		}
	}
	return a.summarize(total)
}

func (a *Accountant) summarize(tierMap map[ModelTier]uint64) UsageSummary {
	s := UsageSummary{ByTier: make(map[ModelTier]uint64)}
	for tier, tokens := range tierMap {
		s.ByTier[tier] = tokens
		s.TotalTokens += tokens
	}
	return s
}

// RecordCount returns the total number of usage records (for testing).
func (a *Accountant) RecordCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return len(a.records)
}
