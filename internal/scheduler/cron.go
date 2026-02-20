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
	CronSpawn   CronAction = iota // spawn a new process
	CronWake                      // wake a sleeping process
	CronExecute                   // execute task on existing process
	CronMessage                   // send IPC message to existing process
)

func (a CronAction) String() string {
	switch a {
	case CronSpawn:
		return "spawn"
	case CronWake:
		return "wake"
	case CronExecute:
		return "execute"
	case CronMessage:
		return "message"
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

	// For CronExecute — task to execute on TargetPID.
	ExecuteDesc   string            // task description
	ExecuteParams map[string]string // task params

	LastRun  time.Time
	NextRun  time.Time
	Enabled  bool

	// Execution history (updated after each run).
	LastExitCode  int32
	LastOutput    string
	LastDurationMs int64
}

// CronSchedule represents a parsed cron expression.
// Supports 6-field Quartz-style: second minute hour day-of-month month day-of-week
// and 5-field Unix-style: minute hour day-of-month month day-of-week (second defaults to 0).
// Supports: *, specific values, comma-separated values, and intervals (*/N).
type CronSchedule struct {
	Second     cronField
	Minute     cronField
	Hour       cronField
	DayOfMonth cronField
	Month      cronField
	DayOfWeek  cronField
	Raw        string
}

type cronField struct {
	Any      bool // *
	Values   []int
	Interval int // */N
}

// ParseCron parses a cron expression.
// 6 fields (Quartz-style): "second minute hour day month dow"
// 5 fields (Unix-style):   "minute hour day month dow" (second defaults to 0)
func ParseCron(expr string) (CronSchedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 && len(parts) != 6 {
		return CronSchedule{}, fmt.Errorf("cron: need 5 or 6 fields, got %d: %q", len(parts), expr)
	}

	// 6 fields: second minute hour day month dow
	// 5 fields: minute hour day month dow (second defaults to 0)
	offset := 0
	var second cronField
	if len(parts) == 6 {
		s, err := parseCronField(parts[0])
		if err != nil {
			return CronSchedule{}, fmt.Errorf("field 0 (%q): %w", parts[0], err)
		}
		second = s
		offset = 1
	} else {
		second = cronField{Values: []int{0}}
	}

	fields := make([]cronField, 5)
	for i := 0; i < 5; i++ {
		f, err := parseCronField(parts[offset+i])
		if err != nil {
			return CronSchedule{}, fmt.Errorf("field %d (%q): %w", offset+i, parts[offset+i], err)
		}
		fields[i] = f
	}

	return CronSchedule{
		Second:     second,
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
	return cs.Second.matches(t.Second()) &&
		cs.Minute.matches(t.Minute()) &&
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
// and haven't been run in the current second.
func (cs *CronScheduler) CheckDue(now time.Time) []*CronEntry {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var due []*CronEntry
	// Truncate to second for comparison (second-precision cron).
	nowSecond := now.Truncate(time.Second)

	for _, entry := range cs.entries {
		if !entry.Enabled {
			continue
		}
		if !entry.Schedule.Matches(now) {
			continue
		}
		// Don't re-trigger within the same second.
		if entry.LastRun.Truncate(time.Second).Equal(nowSecond) {
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

// ParseAndAdd parses a cron expression and adds a new entry in one step.
// action should be "execute", "spawn", or "wake".
func (cs *CronScheduler) ParseAndAdd(
	name, cronExpr, action string,
	targetPID process.PID,
	executeDesc string,
	executeParams map[string]string,
) (string, error) {
	sched, err := ParseCron(cronExpr)
	if err != nil {
		return "", fmt.Errorf("invalid cron expression: %w", err)
	}

	var cronAction CronAction
	switch action {
	case "execute", "":
		cronAction = CronExecute
	case "spawn":
		cronAction = CronSpawn
	case "wake":
		cronAction = CronWake
	case "message":
		cronAction = CronMessage
	default:
		return "", fmt.Errorf("unknown cron action: %q", action)
	}

	entry := &CronEntry{
		Name:          name,
		Schedule:      sched,
		Action:        cronAction,
		TargetPID:     targetPID,
		ExecuteDesc:   executeDesc,
		ExecuteParams: executeParams,
	}
	id := cs.Add(entry)
	return id, nil
}

// Count returns the number of cron entries.
func (cs *CronScheduler) Count() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.entries)
}

// NextRunAfter finds the next time after 'after' when the schedule matches.
// Scans up to 86400 seconds (24 hours) ahead. Returns zero time if not found.
func NextRunAfter(sched CronSchedule, after time.Time) time.Time {
	// Start from the next second boundary.
	t := after.Truncate(time.Second).Add(time.Second)
	for i := 0; i < 86400; i++ {
		if sched.Matches(t) {
			return t
		}
		t = t.Add(time.Second)
	}
	return time.Time{}
}
