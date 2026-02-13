package resources

import (
	"fmt"
	"sync"

	"github.com/selemilka/hivekernel/internal/process"
)

// ModelTier identifies a model pricing tier for budget tracking.
type ModelTier string

const (
	TierOpus   ModelTier = "opus"
	TierSonnet ModelTier = "sonnet"
	TierMini   ModelTier = "mini"
)

// TierFromCog maps cognitive tier to model tier.
func TierFromCog(cog process.CognitiveTier) ModelTier {
	switch cog {
	case process.CogStrategic:
		return TierOpus
	case process.CogTactical:
		return TierSonnet
	default:
		return TierMini
	}
}

// BudgetEntry tracks token budget for a single process.
type BudgetEntry struct {
	PID       process.PID
	Tier      ModelTier
	Allocated uint64 // total tokens allocated by parent
	Consumed  uint64 // tokens consumed so far
	Reserved  uint64 // tokens reserved for children's allocations
}

// Remaining returns the usable token budget (not yet consumed or reserved).
func (b *BudgetEntry) Remaining() uint64 {
	used := b.Consumed + b.Reserved
	if used >= b.Allocated {
		return 0
	}
	return b.Allocated - used
}

// BudgetManager tracks token budgets across the process tree.
// Budgets are inherited: parent allocates from its own pool to children.
// When a child dies, unused budget returns to parent.
type BudgetManager struct {
	mu       sync.RWMutex
	budgets  map[budgetKey]*BudgetEntry
	registry *process.Registry
}

type budgetKey struct {
	pid  process.PID
	tier ModelTier
}

// NewBudgetManager creates a budget manager backed by the process registry.
func NewBudgetManager(registry *process.Registry) *BudgetManager {
	return &BudgetManager{
		budgets:  make(map[budgetKey]*BudgetEntry),
		registry: registry,
	}
}

// SetBudget sets the total token allocation for a process at a given tier.
// This is typically called when spawning the kernel or top-level agents.
func (bm *BudgetManager) SetBudget(pid process.PID, tier ModelTier, tokens uint64) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	key := budgetKey{pid, tier}
	if e, ok := bm.budgets[key]; ok {
		e.Allocated = tokens
	} else {
		bm.budgets[key] = &BudgetEntry{
			PID:       pid,
			Tier:      tier,
			Allocated: tokens,
		}
	}
}

// GetBudget returns the budget entry for a process at a given tier, or nil.
func (bm *BudgetManager) GetBudget(pid process.PID, tier ModelTier) *BudgetEntry {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	return bm.budgets[budgetKey{pid, tier}]
}

// Allocate transfers tokens from parent's budget to a new child.
// Validates: parent has enough remaining budget.
// Returns error if parent doesn't have sufficient tokens.
func (bm *BudgetManager) Allocate(parentPID, childPID process.PID, tier ModelTier, tokens uint64) error {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	parentKey := budgetKey{parentPID, tier}
	parent, ok := bm.budgets[parentKey]
	if !ok {
		return fmt.Errorf("parent PID %d has no %s budget", parentPID, tier)
	}

	if parent.Remaining() < tokens {
		return fmt.Errorf(
			"parent PID %d has %d %s tokens remaining, child needs %d",
			parentPID, parent.Remaining(), tier, tokens,
		)
	}

	// Reserve tokens from parent.
	parent.Reserved += tokens

	// Create child budget.
	childKey := budgetKey{childPID, tier}
	bm.budgets[childKey] = &BudgetEntry{
		PID:       childPID,
		Tier:      tier,
		Allocated: tokens,
	}

	return nil
}

// Consume records token usage against a process's budget.
// Returns error if the process would exceed its allocation.
func (bm *BudgetManager) Consume(pid process.PID, tier ModelTier, tokens uint64) error {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	key := budgetKey{pid, tier}
	entry, ok := bm.budgets[key]
	if !ok {
		return fmt.Errorf("PID %d has no %s budget", pid, tier)
	}

	if entry.Consumed+tokens > entry.Allocated-entry.Reserved {
		return fmt.Errorf(
			"PID %d budget exceeded: consumed=%d + request=%d > available=%d",
			pid, entry.Consumed, tokens, entry.Allocated-entry.Reserved,
		)
	}

	entry.Consumed += tokens
	return nil
}

// Release returns a child's unused budget to its parent.
// Called when a child process dies. Removes the child's budget entry.
func (bm *BudgetManager) Release(childPID process.PID, tier ModelTier) (returned uint64, err error) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	childKey := budgetKey{childPID, tier}
	child, ok := bm.budgets[childKey]
	if !ok {
		return 0, nil // no budget to release
	}

	unused := child.Allocated - child.Consumed
	delete(bm.budgets, childKey)

	// Return unused tokens to parent.
	proc, pErr := bm.registry.Get(childPID)
	if pErr != nil {
		return unused, nil // orphan, nothing to return to
	}

	parentKey := budgetKey{proc.PPID, tier}
	parent, ok := bm.budgets[parentKey]
	if !ok {
		return unused, nil // parent has no budget at this tier
	}

	// Unreserve the full allocation, not just unused (the allocation was reserved).
	if parent.Reserved >= child.Allocated {
		parent.Reserved -= child.Allocated
	} else {
		parent.Reserved = 0
	}

	return unused, nil
}

// ReleaseAll releases all budget tiers for a child process.
func (bm *BudgetManager) ReleaseAll(childPID process.PID) {
	for _, tier := range []ModelTier{TierOpus, TierSonnet, TierMini} {
		bm.Release(childPID, tier)
	}
}

// BranchUsage returns total consumed tokens across a subtree.
func (bm *BudgetManager) BranchUsage(rootPID process.PID, tier ModelTier) uint64 {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	var total uint64
	if e, ok := bm.budgets[budgetKey{rootPID, tier}]; ok {
		total += e.Consumed
	}

	descendants := bm.registry.GetDescendants(rootPID)
	for _, d := range descendants {
		if e, ok := bm.budgets[budgetKey{d.PID, tier}]; ok {
			total += e.Consumed
		}
	}
	return total
}
