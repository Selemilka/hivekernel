package scheduler

import (
	"testing"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

func TestParseCron_Basic(t *testing.T) {
	sched, err := ParseCron("0 8 * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if sched.Raw != "0 8 * * *" {
		t.Errorf("Raw=%q", sched.Raw)
	}
	if sched.Minute.Any || len(sched.Minute.Values) != 1 || sched.Minute.Values[0] != 0 {
		t.Error("minute should be 0")
	}
	if sched.Hour.Any || len(sched.Hour.Values) != 1 || sched.Hour.Values[0] != 8 {
		t.Error("hour should be 8")
	}
	if !sched.DayOfMonth.Any {
		t.Error("day of month should be *")
	}
}

func TestParseCron_Interval(t *testing.T) {
	sched, err := ParseCron("*/5 * * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if sched.Minute.Interval != 5 {
		t.Errorf("minute interval=%d, want 5", sched.Minute.Interval)
	}
}

func TestParseCron_MultiValue(t *testing.T) {
	sched, err := ParseCron("0,30 * * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if len(sched.Minute.Values) != 2 {
		t.Errorf("minute values=%d, want 2", len(sched.Minute.Values))
	}
}

func TestParseCron_Invalid(t *testing.T) {
	if _, err := ParseCron("invalid"); err == nil {
		t.Error("should fail on invalid expression")
	}
	if _, err := ParseCron("0 8 *"); err == nil {
		t.Error("should fail with only 3 fields")
	}
	if _, err := ParseCron("* * * * * * *"); err == nil {
		t.Error("should fail with 7 fields")
	}
	if _, err := ParseCron("*/0 * * * *"); err == nil {
		t.Error("should fail on */0")
	}
}

func TestCronSchedule_Matches(t *testing.T) {
	// "0 8 * * *" — every day at 08:00.
	sched, _ := ParseCron("0 8 * * *")

	match := time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	if !sched.Matches(match) {
		t.Error("should match 08:00")
	}

	noMatch := time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)
	if sched.Matches(noMatch) {
		t.Error("should not match 09:00")
	}
}

func TestCronSchedule_Matches_Interval(t *testing.T) {
	// "*/15 * * * *" — every 15 minutes.
	sched, _ := ParseCron("*/15 * * * *")

	for _, min := range []int{0, 15, 30, 45} {
		t1 := time.Date(2025, 1, 15, 10, min, 0, 0, time.UTC)
		if !sched.Matches(t1) {
			t.Errorf("should match minute %d", min)
		}
	}

	noMatch := time.Date(2025, 1, 15, 10, 7, 0, 0, time.UTC)
	if sched.Matches(noMatch) {
		t.Error("should not match minute 7")
	}
}

func TestCronSchedule_Matches_DayOfWeek(t *testing.T) {
	// "0 9 * * 1" — every Monday at 09:00.
	sched, _ := ParseCron("0 9 * * 1")

	// 2025-01-13 is a Monday.
	monday := time.Date(2025, 1, 13, 9, 0, 0, 0, time.UTC)
	if !sched.Matches(monday) {
		t.Error("should match Monday 09:00")
	}

	tuesday := time.Date(2025, 1, 14, 9, 0, 0, 0, time.UTC)
	if sched.Matches(tuesday) {
		t.Error("should not match Tuesday")
	}
}

func TestCronScheduler_AddAndGet(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("0 8 * * *")

	id := cs.Add(&CronEntry{
		Name:     "morning-check",
		Schedule: sched,
		Action:   CronSpawn,
		VPS:      "vps1",
	})

	if id == "" {
		t.Error("ID should not be empty")
	}

	entry, err := cs.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry.Name != "morning-check" {
		t.Errorf("Name=%q, want morning-check", entry.Name)
	}
	if !entry.Enabled {
		t.Error("should be enabled by default")
	}
}

func TestCronScheduler_Remove(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("0 8 * * *")

	id := cs.Add(&CronEntry{Name: "test", Schedule: sched})

	if err := cs.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if cs.Count() != 0 {
		t.Errorf("Count=%d, want 0", cs.Count())
	}

	if err := cs.Remove("nonexistent"); err == nil {
		t.Error("remove nonexistent should fail")
	}
}

func TestCronScheduler_SetEnabled(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("0 8 * * *")

	id := cs.Add(&CronEntry{Name: "test", Schedule: sched})
	cs.SetEnabled(id, false)

	entry, _ := cs.Get(id)
	if entry.Enabled {
		t.Error("should be disabled")
	}

	cs.SetEnabled(id, true)
	entry, _ = cs.Get(id)
	if !entry.Enabled {
		t.Error("should be enabled")
	}
}

func TestCronScheduler_CheckDue(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("0 8 * * *")

	cs.Add(&CronEntry{
		Name:     "morning",
		Schedule: sched,
		Action:   CronWake,
		TargetPID: 100,
	})

	// Check at 08:00:00 — should trigger (5-field cron defaults second to 0).
	at8 := time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	due := cs.CheckDue(at8)
	if len(due) != 1 {
		t.Errorf("due=%d, want 1 at 08:00", len(due))
	}

	// Check again at same minute — should NOT trigger again.
	due2 := cs.CheckDue(at8)
	if len(due2) != 0 {
		t.Errorf("due=%d, want 0 (already triggered this minute)", len(due2))
	}

	// Check at 09:00 — should not trigger.
	at9 := time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)
	due3 := cs.CheckDue(at9)
	if len(due3) != 0 {
		t.Errorf("due=%d, want 0 at 09:00", len(due3))
	}
}

func TestCronScheduler_CheckDue_Disabled(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("0 8 * * *")

	id := cs.Add(&CronEntry{Name: "test", Schedule: sched})
	cs.SetEnabled(id, false)

	at8 := time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	due := cs.CheckDue(at8)
	if len(due) != 0 {
		t.Errorf("disabled entry should not trigger: due=%d", len(due))
	}
}

func TestCronScheduler_ListByVPS(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("* * * * *")

	cs.Add(&CronEntry{Name: "a", Schedule: sched, VPS: "vps1"})
	cs.Add(&CronEntry{Name: "b", Schedule: sched, VPS: "vps2"})
	cs.Add(&CronEntry{Name: "c", Schedule: sched, VPS: "vps1"})

	vps1 := cs.ListByVPS("vps1")
	if len(vps1) != 2 {
		t.Errorf("vps1 entries=%d, want 2", len(vps1))
	}

	vps2 := cs.ListByVPS("vps2")
	if len(vps2) != 1 {
		t.Errorf("vps2 entries=%d, want 1", len(vps2))
	}
}

func TestCronAction_Execute_String(t *testing.T) {
	if CronExecute.String() != "execute" {
		t.Errorf("CronExecute.String()=%q, want execute", CronExecute.String())
	}
}

func TestCronEntry_ExecuteFields(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("*/30 * * * *")

	id := cs.Add(&CronEntry{
		Name:          "github-check",
		Schedule:      sched,
		Action:        CronExecute,
		TargetPID:     42,
		ExecuteDesc:   "Check GitHub repos for updates",
		ExecuteParams: map[string]string{"repos": "[\"org/repo\"]"},
	})

	entry, _ := cs.Get(id)
	if entry.Action != CronExecute {
		t.Errorf("Action=%s, want execute", entry.Action)
	}
	if entry.TargetPID != 42 {
		t.Errorf("TargetPID=%d, want 42", entry.TargetPID)
	}
	if entry.ExecuteDesc != "Check GitHub repos for updates" {
		t.Errorf("ExecuteDesc=%q", entry.ExecuteDesc)
	}
	if entry.ExecuteParams["repos"] != "[\"org/repo\"]" {
		t.Errorf("ExecuteParams=%v", entry.ExecuteParams)
	}
}

func TestCronScheduler_ParseAndAdd(t *testing.T) {
	cs := NewCronScheduler()

	id, err := cs.ParseAndAdd("test-job", "*/5 * * * *", "execute", 10, "do stuff", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("ParseAndAdd: %v", err)
	}
	if id == "" {
		t.Error("ID should not be empty")
	}

	entry, _ := cs.Get(id)
	if entry.Action != CronExecute {
		t.Errorf("Action=%s, want execute", entry.Action)
	}
	if entry.Schedule.Minute.Interval != 5 {
		t.Errorf("minute interval=%d, want 5", entry.Schedule.Minute.Interval)
	}
	if entry.ExecuteDesc != "do stuff" {
		t.Errorf("ExecuteDesc=%q", entry.ExecuteDesc)
	}
}

func TestCronScheduler_ParseAndAdd_DefaultAction(t *testing.T) {
	cs := NewCronScheduler()

	// Empty action defaults to CronExecute.
	id, err := cs.ParseAndAdd("test", "0 8 * * *", "", 10, "desc", nil)
	if err != nil {
		t.Fatalf("ParseAndAdd: %v", err)
	}
	entry, _ := cs.Get(id)
	if entry.Action != CronExecute {
		t.Errorf("Action=%s, want execute (default)", entry.Action)
	}
}

func TestCronScheduler_ParseAndAdd_SpawnAction(t *testing.T) {
	cs := NewCronScheduler()

	id, err := cs.ParseAndAdd("spawn-test", "0 0 * * *", "spawn", 0, "", nil)
	if err != nil {
		t.Fatalf("ParseAndAdd: %v", err)
	}
	entry, _ := cs.Get(id)
	if entry.Action != CronSpawn {
		t.Errorf("Action=%s, want spawn", entry.Action)
	}
}

func TestCronScheduler_ParseAndAdd_InvalidCron(t *testing.T) {
	cs := NewCronScheduler()

	_, err := cs.ParseAndAdd("bad", "not-a-cron", "execute", 10, "", nil)
	if err == nil {
		t.Error("should fail on invalid cron expression")
	}
}

func TestCronScheduler_ParseAndAdd_InvalidAction(t *testing.T) {
	cs := NewCronScheduler()

	_, err := cs.ParseAndAdd("bad", "* * * * *", "invalid", 10, "", nil)
	if err == nil {
		t.Error("should fail on invalid action")
	}
}

func TestCronScheduler_CheckDue_Execute(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("*/5 * * * *")

	cs.Add(&CronEntry{
		Name:          "periodic-check",
		Schedule:      sched,
		Action:        CronExecute,
		TargetPID:     99,
		ExecuteDesc:   "check",
		ExecuteParams: map[string]string{"mode": "fast"},
	})

	// Minute 10 matches */5.
	at := time.Date(2025, 6, 1, 12, 10, 0, 0, time.UTC)
	due := cs.CheckDue(at)
	if len(due) != 1 {
		t.Fatalf("due=%d, want 1", len(due))
	}
	if due[0].Action != CronExecute {
		t.Errorf("Action=%s, want execute", due[0].Action)
	}
	if due[0].ExecuteParams["mode"] != "fast" {
		t.Errorf("params=%v", due[0].ExecuteParams)
	}
}

func TestCronAction_Message_String(t *testing.T) {
	if CronMessage.String() != "message" {
		t.Errorf("CronMessage.String()=%q, want message", CronMessage.String())
	}
}

func TestCronScheduler_ParseAndAdd_MessageAction(t *testing.T) {
	cs := NewCronScheduler()

	id, err := cs.ParseAndAdd("msg-test", "*/10 * * * *", "message", 42, "run health check", map[string]string{"scope": "full"})
	if err != nil {
		t.Fatalf("ParseAndAdd: %v", err)
	}
	if id == "" {
		t.Error("ID should not be empty")
	}

	entry, _ := cs.Get(id)
	if entry.Action != CronMessage {
		t.Errorf("Action=%s, want message", entry.Action)
	}
	if entry.TargetPID != 42 {
		t.Errorf("TargetPID=%d, want 42", entry.TargetPID)
	}
	if entry.ExecuteDesc != "run health check" {
		t.Errorf("ExecuteDesc=%q", entry.ExecuteDesc)
	}
	if entry.ExecuteParams["scope"] != "full" {
		t.Errorf("ExecuteParams=%v", entry.ExecuteParams)
	}
}

func TestCronScheduler_SpawnEntry(t *testing.T) {
	cs := NewCronScheduler()
	sched, _ := ParseCron("0 */2 * * *")

	id := cs.Add(&CronEntry{
		Name:      "spawn-checker",
		Schedule:  sched,
		Action:    CronSpawn,
		SpawnName: "price-checker",
		SpawnRole: process.RoleTask,
		SpawnTier: process.CogOperational,
		SpawnParent: 100,
		KeepAlive: false,
		VPS:       "vps2",
	})

	entry, _ := cs.Get(id)
	if entry.Action != CronSpawn {
		t.Errorf("Action=%s, want spawn", entry.Action)
	}
	if entry.SpawnRole != process.RoleTask {
		t.Error("SpawnRole should be task")
	}
	if entry.KeepAlive {
		t.Error("KeepAlive should be false")
	}
}

// --- 6-field (Quartz-style) cron tests ---

func TestParseCron_SixField(t *testing.T) {
	// "0 */5 * * * *" = second=0, minute=every 5, rest=any
	sched, err := ParseCron("0 */5 * * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if sched.Raw != "0 */5 * * * *" {
		t.Errorf("Raw=%q", sched.Raw)
	}
	if len(sched.Second.Values) != 1 || sched.Second.Values[0] != 0 {
		t.Errorf("Second should be [0], got Values=%v Any=%v Interval=%d", sched.Second.Values, sched.Second.Any, sched.Second.Interval)
	}
	if sched.Minute.Interval != 5 {
		t.Errorf("Minute.Interval=%d, want 5", sched.Minute.Interval)
	}
	if !sched.Hour.Any || !sched.DayOfMonth.Any || !sched.Month.Any || !sched.DayOfWeek.Any {
		t.Error("remaining fields should all be *")
	}
}

func TestParseCron_SixField_SecondInterval(t *testing.T) {
	// "*/10 * * * * *" = every 10 seconds
	sched, err := ParseCron("*/10 * * * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if sched.Second.Interval != 10 {
		t.Errorf("Second.Interval=%d, want 10", sched.Second.Interval)
	}
	if !sched.Minute.Any {
		t.Error("Minute should be *")
	}
}

func TestCronSchedule_Matches_Second(t *testing.T) {
	// "*/15 * * * * *" = every 15 seconds
	sched, _ := ParseCron("*/15 * * * * *")

	for _, sec := range []int{0, 15, 30, 45} {
		tm := time.Date(2025, 1, 15, 10, 5, sec, 0, time.UTC)
		if !sched.Matches(tm) {
			t.Errorf("should match second %d", sec)
		}
	}

	noMatch := time.Date(2025, 1, 15, 10, 5, 7, 0, time.UTC)
	if sched.Matches(noMatch) {
		t.Error("should not match second 7")
	}
}

func TestParseCron_FiveField_DefaultSecond(t *testing.T) {
	// 5-field expression should get Second.Values=[0]
	sched, err := ParseCron("*/5 * * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if len(sched.Second.Values) != 1 || sched.Second.Values[0] != 0 {
		t.Errorf("5-field Second should default to [0], got Values=%v Any=%v Interval=%d",
			sched.Second.Values, sched.Second.Any, sched.Second.Interval)
	}
	// The first field should be parsed as Minute
	if sched.Minute.Interval != 5 {
		t.Errorf("Minute.Interval=%d, want 5", sched.Minute.Interval)
	}
}

func TestCheckDue_SecondPrecision(t *testing.T) {
	cs := NewCronScheduler()
	// "*/10 * * * * *" = every 10 seconds
	sched, _ := ParseCron("*/10 * * * * *")

	cs.Add(&CronEntry{
		Name:     "every-10s",
		Schedule: sched,
		Action:   CronWake,
		TargetPID: 100,
	})

	// Fire at second 0
	at0 := time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	due := cs.CheckDue(at0)
	if len(due) != 1 {
		t.Fatalf("due=%d, want 1 at second 0", len(due))
	}

	// Same second again -- should NOT fire (dedup)
	due2 := cs.CheckDue(at0)
	if len(due2) != 0 {
		t.Errorf("due=%d, want 0 (same second dedup)", len(due2))
	}

	// Different second (10) -- should fire
	at10 := time.Date(2025, 1, 15, 8, 0, 10, 0, time.UTC)
	due3 := cs.CheckDue(at10)
	if len(due3) != 1 {
		t.Errorf("due=%d, want 1 at second 10", len(due3))
	}

	// Second 5 -- should NOT fire (not matching */10)
	at5 := time.Date(2025, 1, 15, 8, 0, 5, 0, time.UTC)
	due4 := cs.CheckDue(at5)
	if len(due4) != 0 {
		t.Errorf("due=%d, want 0 at second 5 (not matching */10)", len(due4))
	}
}
