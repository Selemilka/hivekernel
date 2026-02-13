package resources

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func setupBudgetTree(t *testing.T) (*process.Registry, *BudgetManager) {
	t.Helper()
	r := process.NewRegistry()

	// king(1) -> queen(2) -> worker(3)
	r.Register(&process.Process{Name: "king", User: "root", Role: process.RoleKernel, State: process.StateRunning})
	r.Register(&process.Process{PPID: 1, Name: "queen", User: "root", Role: process.RoleDaemon, State: process.StateRunning})
	r.Register(&process.Process{PPID: 2, Name: "worker", User: "root", Role: process.RoleWorker, State: process.StateRunning})

	bm := NewBudgetManager(r)
	return r, bm
}

func TestBudgetSetAndGet(t *testing.T) {
	_, bm := setupBudgetTree(t)

	bm.SetBudget(1, TierOpus, 100000)

	entry := bm.GetBudget(1, TierOpus)
	if entry == nil {
		t.Fatal("expected budget entry")
	}
	if entry.Allocated != 100000 {
		t.Fatalf("expected 100000 allocated, got %d", entry.Allocated)
	}
	if entry.Remaining() != 100000 {
		t.Fatalf("expected 100000 remaining, got %d", entry.Remaining())
	}
}

func TestBudgetAllocateToChild(t *testing.T) {
	_, bm := setupBudgetTree(t)

	bm.SetBudget(1, TierOpus, 100000)

	// King allocates 50000 to queen.
	err := bm.Allocate(1, 2, TierOpus, 50000)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	// King should have 50000 remaining (100000 - 50000 reserved).
	king := bm.GetBudget(1, TierOpus)
	if king.Remaining() != 50000 {
		t.Fatalf("king remaining: expected 50000, got %d", king.Remaining())
	}
	if king.Reserved != 50000 {
		t.Fatalf("king reserved: expected 50000, got %d", king.Reserved)
	}

	// Queen should have 50000 allocated.
	queen := bm.GetBudget(2, TierOpus)
	if queen == nil || queen.Allocated != 50000 {
		t.Fatal("queen should have 50000 allocated")
	}
}

func TestBudgetAllocateInsufficientFunds(t *testing.T) {
	_, bm := setupBudgetTree(t)

	bm.SetBudget(1, TierOpus, 100000)

	// Try to allocate more than available.
	err := bm.Allocate(1, 2, TierOpus, 200000)
	if err == nil {
		t.Fatal("expected error for insufficient budget")
	}
}

func TestBudgetConsume(t *testing.T) {
	_, bm := setupBudgetTree(t)

	bm.SetBudget(2, TierSonnet, 50000)

	// Consume tokens.
	err := bm.Consume(2, TierSonnet, 10000)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	entry := bm.GetBudget(2, TierSonnet)
	if entry.Consumed != 10000 {
		t.Fatalf("expected 10000 consumed, got %d", entry.Consumed)
	}
	if entry.Remaining() != 40000 {
		t.Fatalf("expected 40000 remaining, got %d", entry.Remaining())
	}
}

func TestBudgetConsumeExceeded(t *testing.T) {
	_, bm := setupBudgetTree(t)

	bm.SetBudget(2, TierSonnet, 1000)

	err := bm.Consume(2, TierSonnet, 2000)
	if err == nil {
		t.Fatal("expected error for budget exceeded")
	}
}

func TestBudgetRelease(t *testing.T) {
	_, bm := setupBudgetTree(t)

	bm.SetBudget(1, TierOpus, 100000)
	bm.Allocate(1, 2, TierOpus, 50000)

	// Queen consumes 10000.
	bm.Consume(2, TierOpus, 10000)

	// Queen dies — release budget back to king.
	returned, err := bm.Release(2, TierOpus)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if returned != 40000 {
		t.Fatalf("expected 40000 returned, got %d", returned)
	}

	// King should have full 100000 remaining (reservation cleared).
	king := bm.GetBudget(1, TierOpus)
	if king.Reserved != 0 {
		t.Fatalf("king reserved should be 0, got %d", king.Reserved)
	}
	if king.Remaining() != 100000 {
		t.Fatalf("king remaining: expected 100000, got %d", king.Remaining())
	}
}

func TestBudgetBranchUsage(t *testing.T) {
	_, bm := setupBudgetTree(t)

	bm.SetBudget(1, TierOpus, 100000)
	bm.Allocate(1, 2, TierOpus, 50000)
	bm.Allocate(2, 3, TierOpus, 20000)

	bm.Consume(2, TierOpus, 5000)
	bm.Consume(3, TierOpus, 3000)

	// Branch from queen(2) should include queen + worker.
	usage := bm.BranchUsage(2, TierOpus)
	if usage != 8000 {
		t.Fatalf("expected branch usage 8000, got %d", usage)
	}
}

func TestBudgetTierFromCog(t *testing.T) {
	if TierFromCog(process.CogStrategic) != TierOpus {
		t.Fatal("strategic should map to opus")
	}
	if TierFromCog(process.CogTactical) != TierSonnet {
		t.Fatal("tactical should map to sonnet")
	}
	if TierFromCog(process.CogOperational) != TierMini {
		t.Fatal("operational should map to mini")
	}
}
