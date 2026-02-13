package permissions

import (
	"fmt"

	"github.com/selemilka/hivekernel/internal/process"
)

// Capability represents a specific system capability.
type Capability string

const (
	CapSpawnChildren  Capability = "spawn_children"
	CapKillProcesses  Capability = "kill_processes"
	CapManageTree     Capability = "manage_tree"     // reparent, migrate
	CapShellExec      Capability = "shell_exec"      // execute shell commands
	CapNetworkAccess  Capability = "network_access"  // make HTTP requests
	CapFileWrite      Capability = "file_write"       // write to filesystem
	CapFileRead       Capability = "file_read"        // read from filesystem
	CapBudgetAllocate Capability = "budget_allocate"  // allocate budget to children
	CapEscalate       Capability = "escalate"         // escalate to parent
	CapBroadcast      Capability = "broadcast"        // publish events
)

// roleCapabilities defines which capabilities each role has.
var roleCapabilities = map[process.AgentRole][]Capability{
	process.RoleKernel: {
		CapSpawnChildren, CapKillProcesses, CapManageTree,
		CapShellExec, CapNetworkAccess, CapFileWrite, CapFileRead,
		CapBudgetAllocate, CapEscalate, CapBroadcast,
	},
	process.RoleDaemon: {
		CapSpawnChildren, CapKillProcesses, CapManageTree,
		CapNetworkAccess, CapFileRead,
		CapBudgetAllocate, CapEscalate, CapBroadcast,
	},
	process.RoleAgent: {
		CapSpawnChildren, CapKillProcesses,
		CapNetworkAccess, CapFileRead, CapFileWrite,
		CapBudgetAllocate, CapEscalate,
	},
	process.RoleArchitect: {
		CapFileRead, CapFileWrite,
		CapEscalate,
	},
	process.RoleLead: {
		CapSpawnChildren, CapKillProcesses,
		CapNetworkAccess, CapFileRead, CapFileWrite,
		CapBudgetAllocate, CapEscalate,
	},
	process.RoleWorker: {
		CapSpawnChildren, // limited
		CapNetworkAccess, CapFileRead, CapFileWrite,
		CapEscalate,
	},
	process.RoleTask: {
		// Tasks: minimal capabilities.
		// From spec: "task cannot have shell_exec"
		CapFileRead,
		CapEscalate,
	},
}

// CapabilityChecker validates role-based capabilities.
type CapabilityChecker struct {
	registry *process.Registry
	// overrides allows per-process capability grants/revocations.
	grants  map[process.PID]map[Capability]bool
}

// NewCapabilityChecker creates a capability checker backed by the registry.
func NewCapabilityChecker(registry *process.Registry) *CapabilityChecker {
	return &CapabilityChecker{
		registry: registry,
		grants:   make(map[process.PID]map[Capability]bool),
	}
}

// HasCapability checks if a process has a specific capability based on its role.
func (cc *CapabilityChecker) HasCapability(pid process.PID, cap Capability) (bool, error) {
	proc, err := cc.registry.Get(pid)
	if err != nil {
		return false, fmt.Errorf("capability check: %w", err)
	}

	// Check per-process overrides first.
	if grants, ok := cc.grants[pid]; ok {
		if allowed, exists := grants[cap]; exists {
			return allowed, nil
		}
	}

	// Check role-based capabilities.
	caps, ok := roleCapabilities[proc.Role]
	if !ok {
		return false, nil
	}

	for _, c := range caps {
		if c == cap {
			return true, nil
		}
	}
	return false, nil
}

// RequireCapability is like HasCapability but returns an error if the capability is missing.
func (cc *CapabilityChecker) RequireCapability(pid process.PID, cap Capability) error {
	has, err := cc.HasCapability(pid, cap)
	if err != nil {
		return err
	}
	if !has {
		proc, _ := cc.registry.Get(pid)
		role := "unknown"
		if proc != nil {
			role = proc.Role.String()
		}
		return fmt.Errorf("PID %d (role=%s) lacks capability %s", pid, role, cap)
	}
	return nil
}

// Grant adds a capability to a specific process (override role defaults).
func (cc *CapabilityChecker) Grant(pid process.PID, cap Capability) {
	if cc.grants[pid] == nil {
		cc.grants[pid] = make(map[Capability]bool)
	}
	cc.grants[pid][cap] = true
}

// Revoke removes a capability from a specific process (override role defaults).
func (cc *CapabilityChecker) Revoke(pid process.PID, cap Capability) {
	if cc.grants[pid] == nil {
		cc.grants[pid] = make(map[Capability]bool)
	}
	cc.grants[pid][cap] = false
}

// ValidateTools checks that a process's role permits the requested tools.
// From spec: "task cannot have shell_exec".
func (cc *CapabilityChecker) ValidateTools(pid process.PID, tools []string) error {
	for _, tool := range tools {
		switch tool {
		case "shell_exec", "bash", "terminal":
			if err := cc.RequireCapability(pid, CapShellExec); err != nil {
				return fmt.Errorf("tool %q requires shell_exec capability: %w", tool, err)
			}
		case "file_write", "write_file":
			if err := cc.RequireCapability(pid, CapFileWrite); err != nil {
				return fmt.Errorf("tool %q requires file_write capability: %w", tool, err)
			}
		}
	}
	return nil
}

// ListCapabilities returns all capabilities for a role.
func ListCapabilities(role process.AgentRole) []Capability {
	caps, ok := roleCapabilities[role]
	if !ok {
		return nil
	}
	result := make([]Capability, len(caps))
	copy(result, caps)
	return result
}
