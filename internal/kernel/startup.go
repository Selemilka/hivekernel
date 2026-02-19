package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/selemilka/hivekernel/internal/process"
)

// ClawAgentConfig holds PicoClaw-specific settings for a startup agent.
type ClawAgentConfig struct {
	Workspace           string            `json:"workspace,omitempty"`
	Provider            string            `json:"provider,omitempty"`
	MaxToolIterations   int               `json:"max_tool_iterations,omitempty"`
	RestrictToWorkspace bool              `json:"restrict_to_workspace,omitempty"`
	Channels            []string          `json:"channels,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
}

// ClawConfigToMetadata converts ClawAgentConfig into a flat metadata map
// suitable for passing through SpawnRequest.Metadata and ultimately to
// the PicoClaw process via InitRequest.Config.Metadata.
func ClawConfigToMetadata(cc *ClawAgentConfig) map[string]string {
	if cc == nil {
		return nil
	}
	m := make(map[string]string)
	if cc.Workspace != "" {
		m["claw.workspace"] = cc.Workspace
	}
	if cc.Provider != "" {
		m["claw.provider"] = cc.Provider
	}
	if cc.MaxToolIterations > 0 {
		m["claw.max_tool_iterations"] = fmt.Sprintf("%d", cc.MaxToolIterations)
	}
	if cc.RestrictToWorkspace {
		m["claw.restrict_to_workspace"] = "true"
	}
	if len(cc.Channels) > 0 {
		m["claw.channels"] = strings.Join(cc.Channels, ",")
	}
	for k, v := range cc.Env {
		m["claw.env."+k] = os.ExpandEnv(v)
	}
	return m
}

// StartupAgent describes an agent to spawn at kernel startup.
type StartupAgent struct {
	Name             string            `json:"name"`
	Role             string            `json:"role"`
	CognitiveTier    string            `json:"cognitive_tier"`
	Model            string            `json:"model,omitempty"`
	RuntimeType      string            `json:"runtime_type"`
	RuntimeImage     string            `json:"runtime_image"`
	SystemPrompt     string            `json:"system_prompt,omitempty"`
	SystemPromptFile string            `json:"system_prompt_file,omitempty"`
	Tools            []string          `json:"tools,omitempty"`
	Workspace        string            `json:"workspace,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ClawConfig       *ClawAgentConfig  `json:"claw_config,omitempty"`
	Cron             []StartupCron     `json:"cron,omitempty"`
}

// StartupCron describes a cron entry to create after an agent spawns.
type StartupCron struct {
	Name        string            `json:"name"`
	Expression  string            `json:"expression"`
	Action      string            `json:"action,omitempty"`
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

	// Resolve config base dir for relative paths.
	configDir := filepath.Dir(path)

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

		// Load system prompt from file if specified.
		if a.SystemPromptFile != "" {
			promptPath := a.SystemPromptFile
			if !filepath.IsAbs(promptPath) {
				promptPath = filepath.Join(configDir, promptPath)
			}
			content, err := os.ReadFile(promptPath)
			if err != nil {
				return StartupConfig{}, fmt.Errorf("agent[%d] (%s): read system_prompt_file: %w", i, a.Name, err)
			}
			// system_prompt_file wins over system_prompt
			cfg.Agents[i].SystemPrompt = string(content)
		}

		// Resolve workspace to absolute path and create agent subdir.
		if a.Workspace != "" {
			wsPath := a.Workspace
			if !filepath.IsAbs(wsPath) {
				wsPath = filepath.Join(configDir, wsPath)
			}
			agentDir := filepath.Join(wsPath, a.Name)
			if err := os.MkdirAll(agentDir, 0755); err != nil {
				return StartupConfig{}, fmt.Errorf("agent[%d] (%s): create workspace dir: %w", i, a.Name, err)
			}
			cfg.Agents[i].Workspace = agentDir
		}
	}

	return cfg, nil
}

// ParseRuntimeType normalizes a runtime type string from config.
// Accepts both proto-style ("RUNTIME_PYTHON", "RUNTIME_CLAW") and short ("python", "claw").
func ParseRuntimeType(s string) string {
	switch strings.ToUpper(s) {
	case "RUNTIME_CLAW", "CLAW":
		return "claw"
	case "RUNTIME_PYTHON", "PYTHON", "":
		return "python"
	case "RUNTIME_CUSTOM", "CUSTOM":
		return "custom"
	default:
		return strings.ToLower(s)
	}
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
