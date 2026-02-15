package kernel

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/selemilka/hivekernel/internal/process"
)

// StartupAgent describes an agent to spawn at kernel startup.
type StartupAgent struct {
	Name          string            `json:"name"`
	Role          string            `json:"role"`
	CognitiveTier string           `json:"cognitive_tier"`
	Model         string            `json:"model,omitempty"`
	RuntimeType   string            `json:"runtime_type"`
	RuntimeImage  string            `json:"runtime_image"`
	SystemPrompt  string            `json:"system_prompt,omitempty"`
	Cron          []StartupCron     `json:"cron,omitempty"`
}

// StartupCron describes a cron entry to create after an agent spawns.
type StartupCron struct {
	Name        string            `json:"name"`
	Expression  string            `json:"expression"`
	Description string            `json:"description,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
}

// StartupConfig is the top-level startup configuration.
type StartupConfig struct {
	Agents []StartupAgent `json:"agents"`
}

// LoadStartupConfig reads a JSON startup config from the given path.
// Returns an empty config (no agents) if path is empty.
func LoadStartupConfig(path string) (StartupConfig, error) {
	if path == "" {
		return StartupConfig{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return StartupConfig{}, fmt.Errorf("read startup config: %w", err)
	}

	var cfg StartupConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return StartupConfig{}, fmt.Errorf("parse startup config: %w", err)
	}

	// Validate each agent entry.
	for i, a := range cfg.Agents {
		if a.Name == "" {
			return StartupConfig{}, fmt.Errorf("agent[%d]: name is required", i)
		}
		if a.RuntimeImage == "" {
			return StartupConfig{}, fmt.Errorf("agent[%d] (%s): runtime_image is required", i, a.Name)
		}
		if a.RuntimeType == "" {
			cfg.Agents[i].RuntimeType = "RUNTIME_PYTHON" // default
		}
		if a.Role == "" {
			cfg.Agents[i].Role = "daemon" // default
		}
		if a.CognitiveTier == "" {
			cfg.Agents[i].CognitiveTier = "operational" // default
		}
	}

	return cfg, nil
}

// ParseRole converts a string role to process.AgentRole.
func ParseRole(s string) process.AgentRole {
	switch s {
	case "daemon":
		return process.RoleDaemon
	case "agent":
		return process.RoleAgent
	case "lead":
		return process.RoleLead
	case "worker":
		return process.RoleWorker
	case "task":
		return process.RoleTask
	case "architect":
		return process.RoleArchitect
	default:
		return process.RoleDaemon
	}
}

// parseTier converts a string tier to process.CognitiveTier.
func ParseTier(s string) process.CognitiveTier {
	switch s {
	case "strategic":
		return process.CogStrategic
	case "tactical":
		return process.CogTactical
	case "operational":
		return process.CogOperational
	default:
		return process.CogOperational
	}
}
