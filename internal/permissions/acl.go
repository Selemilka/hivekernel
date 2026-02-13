package permissions

import (
	"fmt"
	"sync"

	"github.com/selemilka/hivekernel/internal/process"
)

// Action represents a system operation that can be controlled by ACL.
type Action string

const (
	ActionSpawn         Action = "spawn"
	ActionKill          Action = "kill"
	ActionSendMessage   Action = "send_message"
	ActionReadArtifact  Action = "read_artifact"
	ActionWriteArtifact Action = "write_artifact"
	ActionEscalate      Action = "escalate"
	ActionReadProcess   Action = "read_process"
)

// ACLRule defines a permission rule.
type ACLRule struct {
	User   string       // "*" for any user
	Role   process.AgentRole // -1 for any role
	Action Action
	Allow  bool
}

// ACL manages access control rules for the system.
type ACL struct {
	mu       sync.RWMutex
	rules    []ACLRule
	auth     *AuthProvider
	registry *process.Registry
}

// NewACL creates an ACL system with default rules derived from the spec.
func NewACL(registry *process.Registry, auth *AuthProvider) *ACL {
	acl := &ACL{
		auth:     auth,
		registry: registry,
	}
	acl.addDefaultRules()
	return acl
}

// addDefaultRules initializes rules from the HiveKernel spec.
func (acl *ACL) addDefaultRules() {
	// Kernel can do everything.
	acl.rules = append(acl.rules, ACLRule{Role: process.RoleKernel, Action: "*", Allow: true})

	// Daemons can spawn, kill, send messages, read/write artifacts, escalate.
	for _, action := range []Action{ActionSpawn, ActionKill, ActionSendMessage, ActionReadArtifact, ActionWriteArtifact, ActionEscalate, ActionReadProcess} {
		acl.rules = append(acl.rules, ACLRule{Role: process.RoleDaemon, Action: action, Allow: true})
	}

	// Agents can spawn, send messages, read/write artifacts, escalate.
	for _, action := range []Action{ActionSpawn, ActionSendMessage, ActionReadArtifact, ActionWriteArtifact, ActionEscalate, ActionReadProcess} {
		acl.rules = append(acl.rules, ACLRule{Role: process.RoleAgent, Action: action, Allow: true})
	}

	// Leads can spawn, send messages, read/write artifacts, kill (their children).
	for _, action := range []Action{ActionSpawn, ActionKill, ActionSendMessage, ActionReadArtifact, ActionWriteArtifact, ActionEscalate, ActionReadProcess} {
		acl.rules = append(acl.rules, ACLRule{Role: process.RoleLead, Action: action, Allow: true})
	}

	// Workers can send messages, read/write artifacts, escalate.
	for _, action := range []Action{ActionSendMessage, ActionReadArtifact, ActionWriteArtifact, ActionEscalate, ActionReadProcess} {
		acl.rules = append(acl.rules, ACLRule{Role: process.RoleWorker, Action: action, Allow: true})
	}
	// Workers can spawn limited children.
	acl.rules = append(acl.rules, ACLRule{Role: process.RoleWorker, Action: ActionSpawn, Allow: true})

	// Tasks: can only send to parent (enforced by broker), read artifacts, escalate.
	for _, action := range []Action{ActionSendMessage, ActionReadArtifact, ActionEscalate} {
		acl.rules = append(acl.rules, ACLRule{Role: process.RoleTask, Action: action, Allow: true})
	}
	// Tasks cannot spawn.
	acl.rules = append(acl.rules, ACLRule{Role: process.RoleTask, Action: ActionSpawn, Allow: false})

	// Architects can read/write artifacts, escalate.
	for _, action := range []Action{ActionReadArtifact, ActionWriteArtifact, ActionEscalate, ActionReadProcess} {
		acl.rules = append(acl.rules, ACLRule{Role: process.RoleArchitect, Action: action, Allow: true})
	}
}

// AddRule adds a custom ACL rule (appended after defaults).
func (acl *ACL) AddRule(rule ACLRule) {
	acl.mu.Lock()
	defer acl.mu.Unlock()

	acl.rules = append(acl.rules, rule)
}

// Check validates whether a process is allowed to perform an action.
// Returns nil if allowed, error if denied.
func (acl *ACL) Check(pid process.PID, action Action) error {
	acl.mu.RLock()
	defer acl.mu.RUnlock()

	proc, err := acl.registry.Get(pid)
	if err != nil {
		return fmt.Errorf("acl: process %d not found: %w", pid, err)
	}

	// Check rules in reverse order (last match wins for overrides).
	// But for our setup, first explicit match wins.
	for _, rule := range acl.rules {
		if !acl.ruleMatches(rule, proc, action) {
			continue
		}
		if rule.Allow {
			return nil
		}
		return fmt.Errorf("acl: PID %d (role=%s, user=%s) denied action %s", pid, proc.Role, proc.User, action)
	}

	// Default deny.
	return fmt.Errorf("acl: PID %d (role=%s) has no rule for action %s", pid, proc.Role, action)
}

func (acl *ACL) ruleMatches(rule ACLRule, proc *process.Process, action Action) bool {
	// Match role (-1 means any).
	if rule.Role >= 0 && rule.Role != proc.Role {
		return false
	}

	// Match user ("*" or "" means any).
	if rule.User != "" && rule.User != "*" && rule.User != proc.User {
		return false
	}

	// Match action ("*" means any).
	if rule.Action != "*" && rule.Action != action {
		return false
	}

	return true
}

// CheckCrossUser validates that a process can access another user's resources.
// From spec: "Access to other users' resources goes through kernel."
func (acl *ACL) CheckCrossUser(requesterPID, targetPID process.PID) error {
	requester, err := acl.registry.Get(requesterPID)
	if err != nil {
		return err
	}
	target, err := acl.registry.Get(targetPID)
	if err != nil {
		return err
	}

	// Same user always allowed.
	if requester.User == target.User {
		return nil
	}

	// Kernel always allowed.
	if requester.Role == process.RoleKernel {
		return nil
	}

	return fmt.Errorf(
		"acl: PID %d (user=%s) cannot access PID %d (user=%s) — cross-user access requires kernel",
		requesterPID, requester.User, targetPID, target.User,
	)
}
