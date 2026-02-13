package permissions

import (
	"fmt"

	"github.com/selemilka/hivekernel/internal/process"
)

// Identity represents a resolved user identity for a process.
type Identity struct {
	PID  process.PID
	User string
	Role process.AgentRole
}

// AuthProvider resolves identities and validates identity inheritance.
type AuthProvider struct {
	registry *process.Registry
}

// NewAuthProvider creates an auth provider backed by the process registry.
func NewAuthProvider(registry *process.Registry) *AuthProvider {
	return &AuthProvider{registry: registry}
}

// Resolve returns the identity of a process.
func (a *AuthProvider) Resolve(pid process.PID) (Identity, error) {
	proc, err := a.registry.Get(pid)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve identity: %w", err)
	}
	return Identity{
		PID:  proc.PID,
		User: proc.User,
		Role: proc.Role,
	}, nil
}

// ValidateInheritance checks that a child's user matches its parent's user.
// From spec: "USER is inherited down the branch. All children work under parent's identity."
// Exception: kernel (root) can spawn children with any user.
func (a *AuthProvider) ValidateInheritance(parentPID process.PID, childUser string) error {
	parent, err := a.registry.Get(parentPID)
	if err != nil {
		return fmt.Errorf("validate inheritance: %w", err)
	}

	// Kernel can assign any user.
	if parent.Role == process.RoleKernel {
		return nil
	}

	// Non-kernel must inherit user from parent.
	if childUser != "" && childUser != parent.User {
		return fmt.Errorf(
			"user mismatch: child requests user %q but parent PID %d has user %q (only kernel can assign different user)",
			childUser, parentPID, parent.User,
		)
	}

	return nil
}

// IsSameUser checks if two processes belong to the same user.
func (a *AuthProvider) IsSameUser(pidA, pidB process.PID) (bool, error) {
	procA, err := a.registry.Get(pidA)
	if err != nil {
		return false, err
	}
	procB, err := a.registry.Get(pidB)
	if err != nil {
		return false, err
	}
	return procA.User == procB.User, nil
}

// IsKernel checks if a process is the kernel.
func (a *AuthProvider) IsKernel(pid process.PID) bool {
	proc, err := a.registry.Get(pid)
	if err != nil {
		return false
	}
	return proc.Role == process.RoleKernel
}

// GetUser returns the user of a process.
func (a *AuthProvider) GetUser(pid process.PID) (string, error) {
	proc, err := a.registry.Get(pid)
	if err != nil {
		return "", err
	}
	return proc.User, nil
}
