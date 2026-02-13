package process

import (
	"fmt"
	"log"
	"time"
)

// TreeOps provides high-level operations on the process tree.
type TreeOps struct {
	registry *Registry
	signals  *SignalRouter
}

// NewTreeOps creates tree operations backed by the given registry and signal router.
func NewTreeOps(registry *Registry, signals *SignalRouter) *TreeOps {
	return &TreeOps{
		registry: registry,
		signals:  signals,
	}
}

// KillBranch kills a process and all its descendants.
// Kills leaf nodes first (bottom-up) to avoid orphans.
// Each process gets SIGTERM + grace period, then SIGKILL.
func (t *TreeOps) KillBranch(pid PID, grace time.Duration) ([]PID, error) {
	proc, err := t.registry.Get(pid)
	if err != nil {
		return nil, fmt.Errorf("kill branch: %w", err)
	}

	// Collect all descendants in bottom-up order (leaves first).
	descendants := t.registry.GetDescendants(pid)
	ordered := t.bottomUpOrder(pid, descendants)

	var killed []PID

	for _, target := range ordered {
		if target.State == StateDead {
			continue
		}

		log.Printf("[tree] killing PID %d (%s) in branch of PID %d",
			target.PID, target.Name, pid)

		// Send SIGTERM.
		_ = t.signals.Send(target.PID, SIGTERM, nil)
		killed = append(killed, target.PID)
	}

	// Schedule forced kill after grace period for any survivors.
	go func() {
		time.Sleep(grace)
		for _, target := range ordered {
			p, err := t.registry.Get(target.PID)
			if err != nil {
				continue
			}
			if p.State != StateDead {
				log.Printf("[tree] PID %d did not exit, sending SIGKILL", p.PID)
				_ = t.signals.Send(p.PID, SIGKILL, nil)
			}
		}
	}()

	// Also kill the root of the branch.
	if proc.State != StateDead {
		_ = t.signals.Send(pid, SIGTERM, nil)
		killed = append(killed, pid)

		go func() {
			time.Sleep(grace)
			p, err := t.registry.Get(pid)
			if err != nil {
				return
			}
			if p.State != StateDead {
				_ = t.signals.Send(pid, SIGKILL, nil)
			}
		}()
	}

	return killed, nil
}

// bottomUpOrder returns descendants sorted so that leaves come first.
// This ensures children are killed before their parents.
func (t *TreeOps) bottomUpOrder(rootPID PID, descendants []*Process) []*Process {
	// Build depth map.
	depth := make(map[PID]int)
	for _, p := range descendants {
		depth[p.PID] = t.depthFrom(rootPID, p.PID)
	}

	// Sort by depth descending (deepest first).
	sorted := make([]*Process, len(descendants))
	copy(sorted, descendants)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if depth[sorted[i].PID] < depth[sorted[j].PID] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted
}

// depthFrom computes the depth of a PID relative to a root.
func (t *TreeOps) depthFrom(root, pid PID) int {
	d := 0
	cur := pid
	for cur != root {
		p, err := t.registry.Get(cur)
		if err != nil || p.PPID == cur {
			break
		}
		cur = p.PPID
		d++
	}
	return d
}

// Reparent moves a process (and its subtree) under a new parent.
// Used for migration and orphan adoption.
func (t *TreeOps) Reparent(pid, newParentPID PID) error {
	_, err := t.registry.Get(newParentPID)
	if err != nil {
		return fmt.Errorf("reparent: new parent %d not found: %w", newParentPID, err)
	}

	err = t.registry.Update(pid, func(p *Process) {
		oldPPID := p.PPID
		p.PPID = newParentPID
		log.Printf("[tree] reparented PID %d (%s) from PPID %d to PPID %d",
			p.PID, p.Name, oldPPID, newParentPID)
	})
	if err != nil {
		return fmt.Errorf("reparent: %w", err)
	}

	// Notify the process that its parent changed.
	_ = t.signals.Send(pid, SIGHUP, nil)
	return nil
}

// OrphanAdoption finds all orphaned processes (parent is dead/missing)
// and reparents them under the given adoptive parent.
func (t *TreeOps) OrphanAdoption(adoptiveParentPID PID) []PID {
	var adopted []PID

	for _, p := range t.registry.List() {
		if p.PID == adoptiveParentPID || p.PPID == 0 {
			continue
		}
		parent, err := t.registry.Get(p.PPID)
		if err != nil || parent.State == StateDead {
			if err := t.Reparent(p.PID, adoptiveParentPID); err == nil {
				adopted = append(adopted, p.PID)
			}
		}
	}

	if len(adopted) > 0 {
		log.Printf("[tree] adopted %d orphans under PID %d", len(adopted), adoptiveParentPID)
	}
	return adopted
}

// SubtreeVPS returns all processes in a subtree that are on a specific VPS.
func (t *TreeOps) SubtreeVPS(rootPID PID, vps string) []*Process {
	descendants := t.registry.GetDescendants(rootPID)
	var result []*Process
	for _, p := range descendants {
		if p.VPS == vps {
			result = append(result, p)
		}
	}
	return result
}
