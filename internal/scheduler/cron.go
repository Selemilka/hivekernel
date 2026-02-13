package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// CronAction defines what happens when a cron entry triggers.
type CronAction int

const (
	CronSpawn CronAction = iota // spawn a new process
	CronWake                    // wake a sleeping process
)

func (a CronAction) String() string {
	switch a {
	case CronSpawn:
		return "spawn"
	case CronWake:
		return "wake"
	default:
		return "unknown"
	}
}

// CronEntry represents a scheduled recurring action.
type CronEntry struct {
	ID       string
	Name     string
	Schedule CronSchedule
	Action   CronAction
	TargetPID process.PID // for CronWake
	VPS      string       // which VPS this entry belongs to

	// For CronSpawn — the spawn template.
	SpawnName     string
	SpawnRole     process.AgentRole
	SpawnTier     process.CognitiveTier
	SpawnParent   process.PID
	KeepAlive     bool // true = sleep between runs, false = kill and respawn

	LastRun  time.Time
	NextRun  time.Time
	Enabled  bool
}

// CronSchedule represents a parsed cron expression.
// Simplified cron: minute hour day-of-month month day-of-week.
// Supports: *, specific values, and intervals (*/N).
type CronSchedule struct {
	Minute    cronField
	Hour      cronField
	DayOfMonth cronField
	Month     cronField
	DayOfWeek cronField
	Raw       string
}

type cronField struct {
	Any      bool // *
	Values   []int
	Interval int // */N
}

// ParseCron parses a cron expression like "0 8 * * *" or "*/5 * * * *".
func ParseCron(expr string) (CronSchedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return CronSchedule{}, fmt.Errorf("cron expression must have 5 fields, got %d: %q", len(parts), expr)
	}

	fields := make([]cronField, 5)
	for i, part := range parts {
		f, err := parseCronField(part)
		if err != nil {
			return CronSchedule{}, fmt.Errorf("field %d (%q): %w", i, part, err)
		}
		fields[i] = f
	}

	return CronSchedule{
		Minute:     fields[0],
		Hour:       fields[1],
		DayOfMonth: fields[2],
		Month:      fields[3],
		DayOfWeek:  fields[4],
		Raw:        expr,
	}, nil
}

func parseCronField(s string) (cronField, error) {
	if s == "*" {
		return cronField{Any: true}, nil
	}

	if strings.HasPrefix(s, "*/") {
		n, err := strconv.Atoi(s[2:])
		if err != nil || n <= 0 {
			return cronField{}, fmt.Errorf("invalid interval: %q", s)
		}
		return cronField{Interval: n}, nil
	}

	// Comma-separated values.
	parts := strings.Split(s, ",")
	var values []int
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return cronField{}, fmt.Errorf("invalid value: %q", p)
		}
		values = append(values, v)
	}
	return cronField{Values: values}, nil
}

// Matches checks if a time matches this schedule.
func (cs CronSchedule) Matches(t time.Time) bool {
	return cs.Minute.matches(t.Minute()) &&
		cs.Hour.matches(t.Hour()) &&
		cs.DayOfMonth.matches(t.Day()) &&
		cs.Month.matches(int(t.Month())) &&
		cs.DayOfWeek.matches(int(t.Weekday()))
}

func (f cronField) matches(value int) bool {
	if f.Any {
		return true
	}
	if f.Interval > 0 {
		return value%f.Interval == 0
	}
	for _, v := range f.Values {
		if v == value {
			return true
		}
	}
	return false
}

// CronScheduler manages recurring scheduled actions.
// In the HiveKernel model, queen holds the crontab per VPS.
type CronScheduler struct {
	mu      sync.RWMutex
	entries map[string]*CronEntry // id -> entry
	nextID  int
}

// NewCronScheduler creates an empty cron scheduler.
func NewCronScheduler() *CronScheduler {
	return &CronScheduler{
		entries: make(map[string]*CronEntry),
	}
}

// Add adds a new cron entry.
func (cs *CronScheduler) Add(entry *CronEntry) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.nextID++
	entry.ID = fmt.Sprintf("cron-%d", cs.nextID)
	entry.Enabled = true
	cs.entries[entry.ID] = entry
	return entry.ID
}

// Remove removes a cron entry.
func (cs *CronScheduler) Remove(id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if _, ok := cs.entries[id]; !ok {
		return fmt.Errorf("cron entry %q not found", id)
	}
	delete(cs.entries, id)
	return nil
}

// Get returns a cron entry by ID.
func (cs *CronScheduler) Get(id string) (*CronEntry, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	entry, ok := cs.entries[id]
	if !ok {
		return nil, fmt.Errorf("cron entry %q not found", id)
	}
	return entry, nil
}

// Enable or disable a cron entry.
func (cs *CronScheduler) SetEnabled(id string, enabled bool) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	entry, ok := cs.entries[id]
	if !ok {
		return fmt.Errorf("cron entry %q not found", id)
	}
	entry.Enabled = enabled
	return nil
}

// CheckDue returns all entries that are due at the given time
// and haven't been run in the current minute.
func (cs *CronScheduler) CheckDue(now time.Time) []*CronEntry {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var due []*CronEntry
	// Truncate to minute for comparison.
	nowMinute := now.Truncate(time.Minute)

	for _, entry := range cs.entries {
		if !entry.Enabled {
			continue
		}
		if !entry.Schedule.Matches(now) {
			continue
		}
		// Don't re-trigger within the same minute.
		if entry.LastRun.Truncate(time.Minute).Equal(nowMinute) {
			continue
		}

		entry.LastRun = now
		due = append(due, entry)
	}
	return due
}

// List returns all cron entries.
func (cs *CronScheduler) List() []*CronEntry {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	list := make([]*CronEntry, 0, len(cs.entries))
	for _, e := range cs.entries {
		list = append(list, e)
	}
	return list
}

// ListByVPS returns cron entries for a specific VPS.
func (cs *CronScheduler) ListByVPS(vps string) []*CronEntry {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	var list []*CronEntry
	for _, e := range cs.entries {
		if e.VPS == vps {
			list = append(list, e)
		}
	}
	return list
}

// Count returns the number of cron entries.
func (cs *CronScheduler) Count() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.entries)
}
