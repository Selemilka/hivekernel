package scheduler_test

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/resources"
	"github.com/selemilka/hivekernel/internal/scheduler"
)

// TestCompilerScenario simulates the full compiler scenario from the spec:
// Leo spawns leads -> leads spawn workers -> tree grows -> tasks complete -> tree collapses.
//
// Tree shape:
//   king (PID 1) -> queen@vps2 (PID 2) -> Leo (PID 3)
//     -> Architect (PID 4, sleeps after design)
//     -> Frontend-Lead (PID 5)
//         -> Lexer-dev (PID 6)
//         -> Parser-dev (PID 7)
//         -> Lexer-tests (PID 8, task)
//     -> Backend-Lead (PID 9)
//         -> Codegen-x86 (PID 10)
//         -> Codegen-arm (PID 11)
//     -> Testing-Lead (PID 12)
//         -> Unit-runner (PID 13, task)
//         -> Integration-runner (PID 14, task)
func TestCompilerScenario(t *testing.T) {
	reg := process.NewRegistry()
	spawner := process.NewSpawner(reg)
	signals := process.NewSignalRouter(reg)
	lm := process.NewLifecycleManager(reg, signals)
	sched := scheduler.NewScheduler(0.1)
	budget := resources.NewBudgetManager(reg)
	cgroups := resources.NewCGroupManager(reg)

	// --- Phase 1: Bootstrap ---
	king, _ := spawner.SpawnKernel("king", "root", "vps1")
	budget.SetBudget(king.PID, resources.TierOpus, 1000000)
	budget.SetBudget(king.PID, resources.TierSonnet, 10000000)

	queen, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     king.PID,
		Name:          "queen@vps2",
		Role:          process.RoleDaemon,
		CognitiveTier: process.CogTactical,
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 20, MaxTokensTotal: 5000000},
	})
	budget.Allocate(king.PID, queen.PID, resources.TierSonnet, 5000000)

	// --- Phase 2: Leo spawns (strategic agent) ---
	// Note: Leo is tactical here because spec's cognitive tier rule says child <= parent.
	// Queen is tactical, so Leo must be tactical or lower.
	leo, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     queen.PID,
		Name:          "Leo",
		Role:          process.RoleAgent,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 10, MaxTokensTotal: 2000000},
	})
	budget.Allocate(queen.PID, leo.PID, resources.TierSonnet, 2000000)
	_ = reg.SetState(leo.PID, process.StateRunning)

	// Create a cgroup for Leo's entire branch.
	leoGroup, err := cgroups.Create("leo-compiler", leo.PID, 2000000, 15)
	if err != nil {
		t.Fatalf("Create cgroup: %v", err)
	}

	// --- Phase 3: Leo spawns architect (designs, then sleeps) ---
	architect, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     leo.PID,
		Name:          "Architect",
		Role:          process.RoleArchitect,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 0, MaxTokensTotal: 100000},
	})
	_ = reg.SetState(architect.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", architect.PID)

	// Architect produces design artifact and sleeps.
	cgroups.ConsumeTokens(architect.PID, 50000)
	lm.CompleteTask(architect.PID, 0, []byte("compiler design: 4 modules"), "")
	// In real system, architect would sleep instead of die. Let's simulate:
	archProc, _ := reg.Get(architect.PID)
	_ = reg.SetState(archProc.PID, process.StateSleeping)

	// --- Phase 4: Leo spawns leads based on design ---
	frontendLead, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     leo.PID,
		Name:          "Frontend-Lead",
		Role:          process.RoleLead,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 5, MaxTokensTotal: 500000},
	})
	_ = reg.SetState(frontendLead.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", frontendLead.PID)

	backendLead, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     leo.PID,
		Name:          "Backend-Lead",
		Role:          process.RoleLead,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 5, MaxTokensTotal: 500000},
	})
	_ = reg.SetState(backendLead.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", backendLead.PID)

	testingLead, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     leo.PID,
		Name:          "Testing-Lead",
		Role:          process.RoleLead,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 5, MaxTokensTotal: 200000},
	})
	_ = reg.SetState(testingLead.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", testingLead.PID)

	// --- Phase 5: Leads spawn workers (tree grows) ---
	// Frontend-Lead spawns lexer-dev and parser-dev.
	lexerDev, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     frontendLead.PID,
		Name:          "Lexer-dev",
		Role:          process.RoleWorker,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		Limits:        process.ResourceLimits{MaxTokensTotal: 200000},
	})
	_ = reg.SetState(lexerDev.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", lexerDev.PID)

	parserDev, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     frontendLead.PID,
		Name:          "Parser-dev",
		Role:          process.RoleWorker,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		Limits:        process.ResourceLimits{MaxTokensTotal: 200000},
	})
	_ = reg.SetState(parserDev.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", parserDev.PID)

	// Backend-Lead spawns codegen workers.
	codegenX86, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     backendLead.PID,
		Name:          "Codegen-x86",
		Role:          process.RoleWorker,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		Limits:        process.ResourceLimits{MaxTokensTotal: 200000},
	})
	_ = reg.SetState(codegenX86.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", codegenX86.PID)

	codegenArm, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     backendLead.PID,
		Name:          "Codegen-arm",
		Role:          process.RoleWorker,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		Limits:        process.ResourceLimits{MaxTokensTotal: 200000},
	})
	_ = reg.SetState(codegenArm.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", codegenArm.PID)

	// Testing-Lead spawns task runners.
	unitRunner, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     testingLead.PID,
		Name:          "Unit-runner",
		Role:          process.RoleTask,
		CognitiveTier: process.CogOperational,
		User:          "leo",
		Limits:        process.ResourceLimits{MaxTokensTotal: 50000},
	})
	_ = reg.SetState(unitRunner.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", unitRunner.PID)

	integRunner, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     testingLead.PID,
		Name:          "Integration-runner",
		Role:          process.RoleTask,
		CognitiveTier: process.CogOperational,
		User:          "leo",
		Limits:        process.ResourceLimits{MaxTokensTotal: 50000},
	})
	_ = reg.SetState(integRunner.PID, process.StateRunning)
	_ = cgroups.AddProcess("leo-compiler", integRunner.PID)

	// === Verify: tree fully grown ===
	t.Log("=== Tree fully grown ===")

	descendants := reg.GetDescendants(leo.PID)
	// Leo has: architect, 3 leads, 2 frontend workers, 2 backend workers, 2 test tasks = 10
	if len(descendants) != 10 {
		t.Errorf("Leo descendants=%d, want 10", len(descendants))
	}

	// Cgroup should have Leo + 10 descendants = 11.
	if leoGroup.ProcessCount != 11 {
		t.Errorf("cgroup process count=%d, want 11", leoGroup.ProcessCount)
	}

	// Scheduler: submit tasks for the workers.
	sched.Submit("build-lexer", process.CogTactical, process.RoleWorker, leo.PID, nil)
	sched.Submit("build-parser", process.CogTactical, process.RoleWorker, leo.PID, nil)
	sched.Submit("codegen-x86", process.CogTactical, process.RoleWorker, leo.PID, nil)
	sched.Submit("codegen-arm", process.CogTactical, process.RoleWorker, leo.PID, nil)
	sched.Submit("run-unit-tests", process.CogOperational, process.RoleTask, leo.PID, nil)
	sched.Submit("run-integ-tests", process.CogOperational, process.RoleTask, leo.PID, nil)

	if sched.PendingCount() != 6 {
		t.Errorf("pending tasks=%d, want 6", sched.PendingCount())
	}

	// Assign tasks to workers.
	t1 := sched.Assign(lexerDev.PID, process.CogTactical, process.RoleWorker)
	t2 := sched.Assign(parserDev.PID, process.CogTactical, process.RoleWorker)
	t3 := sched.Assign(codegenX86.PID, process.CogTactical, process.RoleWorker)
	t4 := sched.Assign(codegenArm.PID, process.CogTactical, process.RoleWorker)
	t5 := sched.Assign(unitRunner.PID, process.CogOperational, process.RoleTask)
	t6 := sched.Assign(integRunner.PID, process.CogOperational, process.RoleTask)

	if t1 == nil || t2 == nil || t3 == nil || t4 == nil || t5 == nil || t6 == nil {
		t.Fatal("all tasks should be assigned")
	}
	if sched.PendingCount() != 0 {
		t.Errorf("pending after assign=%d, want 0", sched.PendingCount())
	}

	// === Phase 6: Workers complete their tasks ===
	t.Log("=== Workers completing tasks ===")

	// Simulate token consumption through cgroup.
	_ = cgroups.ConsumeTokens(lexerDev.PID, 100000)
	_ = cgroups.ConsumeTokens(parserDev.PID, 80000)
	_ = cgroups.ConsumeTokens(codegenX86.PID, 150000)
	_ = cgroups.ConsumeTokens(codegenArm.PID, 120000)

	// Workers complete.
	lm.CompleteTask(lexerDev.PID, 0, []byte("lexer.wasm"), "")
	lm.CompleteTask(parserDev.PID, 0, []byte("parser.wasm"), "")
	lm.CompleteTask(codegenX86.PID, 0, []byte("x86 binary"), "")
	lm.CompleteTask(codegenArm.PID, 0, []byte("arm binary"), "")

	sched.Complete(t1.ID, true)
	sched.Complete(t2.ID, true)
	sched.Complete(t3.ID, true)
	sched.Complete(t4.ID, true)

	// Test runners complete.
	lm.CompleteTask(unitRunner.PID, 0, []byte("42 tests passed"), "")
	lm.CompleteTask(integRunner.PID, 0, []byte("15 tests passed"), "")
	sched.Complete(t5.ID, true)
	sched.Complete(t6.ID, true)

	completed := sched.TasksByState(scheduler.TaskCompleted)
	if len(completed) != 6 {
		t.Errorf("completed tasks=%d, want 6", len(completed))
	}

	// Collect results from frontend lead's workers.
	lexerResult, ok := lm.WaitResult(lexerDev.PID)
	if !ok || lexerResult.ExitCode != 0 {
		t.Error("lexer result should be available with exit 0")
	}
	parserResult, ok := lm.WaitResult(parserDev.PID)
	if !ok || parserResult.ExitCode != 0 {
		t.Error("parser result should be available with exit 0")
	}

	// === Phase 7: Tree collapses ===
	t.Log("=== Tree collapsing ===")

	// Frontend-Lead collapses its branch (workers are already dead, but cleanup).
	frontendResults, _ := lm.CollapseBranch(frontendLead.PID)
	t.Logf("Frontend-Lead collapsed: %d results", len(frontendResults))

	// Backend-Lead collapses.
	backendResults, _ := lm.CollapseBranch(backendLead.PID)
	t.Logf("Backend-Lead collapsed: %d results", len(backendResults))

	// Testing-Lead collapses.
	testResults, _ := lm.CollapseBranch(testingLead.PID)
	t.Logf("Testing-Lead collapsed: %d results", len(testResults))

	// Leo collapses its entire branch.
	leoResults, _ := lm.CollapseBranch(leo.PID)
	t.Logf("Leo collapsed: %d results", len(leoResults))

	// === Verify: tree collapsed ===
	activeChildren := lm.ActiveChildren(leo.PID)
	if activeChildren != 0 {
		t.Errorf("Leo active children=%d, want 0 (all collapsed)", activeChildren)
	}

	// Check cgroup usage.
	usage, _ := cgroups.GetGroupUsage("leo-compiler")
	t.Logf("Cgroup usage: %d/%d tokens (%.1f%%)",
		usage.TokensConsumed, usage.TokensMax, usage.UsagePercent)

	// Total consumed: architect(50k) + lexer(100k) + parser(80k) + x86(150k) + arm(120k) = 500k
	if usage.TokensConsumed != 500000 {
		t.Errorf("cgroup tokens consumed=%d, want 500000", usage.TokensConsumed)
	}

	t.Log("=== Compiler scenario complete ===")
}
