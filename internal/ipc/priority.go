package ipc

import "github.com/selemilka/hivekernel/internal/process"

// Relationship between sender and receiver, used for base priority calculation.
type Relationship int

const (
	RelSystem  Relationship = iota // From kernel/system
	RelParent                       // From parent
	RelSibling                      // From sibling
	RelChild                        // From child
	RelOther                        // From distant relative (cross-branch)
)

// BasePriorityForRelation returns the base priority adjustment for a message
// based on the sender-receiver relationship.
// Lower value = higher priority. Matches the spec: kernel > parent > sibling > child.
func BasePriorityForRelation(rel Relationship) int {
	switch rel {
	case RelSystem:
		return PriorityCritical // 0
	case RelParent:
		return PriorityHigh // 1
	case RelSibling:
		return PriorityNormal // 2
	case RelChild:
		return PriorityLow // 3
	case RelOther:
		return PriorityLow // 3
	default:
		return PriorityNormal
	}
}

// DetermineRelationship figures out how sender relates to receiver
// using the process registry.
func DetermineRelationship(registry *process.Registry, fromPID, toPID process.PID) Relationship {
	sender, err := registry.Get(fromPID)
	if err != nil {
		return RelOther
	}
	receiver, err := registry.Get(toPID)
	if err != nil {
		return RelOther
	}

	// Kernel is always system-level.
	if sender.Role == process.RoleKernel {
		return RelSystem
	}

	// Parent -> child.
	if receiver.PPID == fromPID {
		return RelParent
	}

	// Child -> parent.
	if sender.PPID == toPID {
		return RelChild
	}

	// Siblings: same parent.
	if sender.PPID == receiver.PPID && sender.PPID != 0 {
		return RelSibling
	}

	return RelOther
}

// ComputeEffectivePriority combines explicit priority, relationship-based
// base priority, and selects the higher (lower numeric) one.
func ComputeEffectivePriority(explicitPriority int, rel Relationship) int {
	relPriority := BasePriorityForRelation(rel)
	// Use the more urgent (lower value) of the two.
	if relPriority < explicitPriority {
		return relPriority
	}
	return explicitPriority
}
