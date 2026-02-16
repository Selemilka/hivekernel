package kernel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func TestLoadStartupConfig_Empty(t *testing.T) {
	cfg, err := LoadStartupConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Agents) != 0 {
		t.Fatalf("expected 0 agents, got %d", len(cfg.Agents))
	}
}

func TestLoadStartupConfig_Valid(t *testing.T) {
	data := `{
		"agents": [
			{
				"name": "queen",
				"role": "daemon",
				"cognitive_tier": "tactical",
				"model": "sonnet",
				"runtime_type": "RUNTIME_PYTHON",
				"runtime_image": "hivekernel_sdk.queen:QueenAgent"
			},
			{
				"name": "monitor",
				"role": "daemon",
				"cognitive_tier": "operational",
				"runtime_type": "RUNTIME_PYTHON",
				"runtime_image": "hivekernel_sdk.monitor:MonitorAgent",
				"cron": [
					{
						"name": "check-repos",
						"expression": "*/30 * * * *",
						"description": "Check repos"
					}
				]
			}
		]
	}`

	path := filepath.Join(t.TempDir(), "startup.json")
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadStartupConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(cfg.Agents))
	}

	// First agent.
	a := cfg.Agents[0]
	if a.Name != "queen" {
		t.Errorf("agent[0] name = %q, want queen", a.Name)
	}
	if a.Model != "sonnet" {
		t.Errorf("agent[0] model = %q, want sonnet", a.Model)
	}

	// Second agent with cron.
	b := cfg.Agents[1]
	if b.Name != "monitor" {
		t.Errorf("agent[1] name = %q, want monitor", b.Name)
	}
	if len(b.Cron) != 1 {
		t.Fatalf("agent[1] expected 1 cron entry, got %d", len(b.Cron))
	}
	if b.Cron[0].Expression != "*/30 * * * *" {
		t.Errorf("cron expression = %q, want */30 * * * *", b.Cron[0].Expression)
	}
}

func TestLoadStartupConfig_Defaults(t *testing.T) {
	data := `{
		"agents": [
			{
				"name": "worker",
				"runtime_image": "my_module:MyAgent"
			}
		]
	}`

	path := filepath.Join(t.TempDir(), "startup.json")
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadStartupConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	a := cfg.Agents[0]
	if a.RuntimeType != "RUNTIME_PYTHON" {
		t.Errorf("default runtime_type = %q, want RUNTIME_PYTHON", a.RuntimeType)
	}
	if a.Role != "daemon" {
		t.Errorf("default role = %q, want daemon", a.Role)
	}
	if a.CognitiveTier != "operational" {
		t.Errorf("default tier = %q, want operational", a.CognitiveTier)
	}
}

func TestLoadStartupConfig_MissingName(t *testing.T) {
	data := `{"agents": [{"runtime_image": "foo:Bar"}]}`
	path := filepath.Join(t.TempDir(), "startup.json")
	os.WriteFile(path, []byte(data), 0644)

	_, err := LoadStartupConfig(path)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestLoadStartupConfig_MissingImage(t *testing.T) {
	data := `{"agents": [{"name": "test"}]}`
	path := filepath.Join(t.TempDir(), "startup.json")
	os.WriteFile(path, []byte(data), 0644)

	_, err := LoadStartupConfig(path)
	if err == nil {
		t.Fatal("expected error for missing runtime_image")
	}
}

func TestLoadStartupConfig_FileNotFound(t *testing.T) {
	_, err := LoadStartupConfig("/nonexistent/path.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadStartupConfig_InvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("{invalid json"), 0644)

	_, err := LoadStartupConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseRole(t *testing.T) {
	tests := []struct {
		input string
		want  process.AgentRole
	}{
		{"daemon", process.RoleDaemon},
		{"agent", process.RoleAgent},
		{"lead", process.RoleLead},
		{"worker", process.RoleWorker},
		{"task", process.RoleTask},
		{"architect", process.RoleArchitect},
		{"unknown", process.RoleDaemon},
	}
	for _, tt := range tests {
		got := ParseRole(tt.input)
		if got != tt.want {
			t.Errorf("ParseRole(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseTier(t *testing.T) {
	tests := []struct {
		input string
		want  process.CognitiveTier
	}{
		{"strategic", process.CogStrategic},
		{"tactical", process.CogTactical},
		{"operational", process.CogOperational},
		{"unknown", process.CogOperational},
	}
	for _, tt := range tests {
		got := ParseTier(tt.input)
		if got != tt.want {
			t.Errorf("ParseTier(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestClawConfigToMetadata_Nil(t *testing.T) {
	m := ClawConfigToMetadata(nil)
	if m != nil {
		t.Errorf("ClawConfigToMetadata(nil) = %v, want nil", m)
	}
}

func TestClawConfigToMetadata_Full(t *testing.T) {
	cc := &ClawAgentConfig{
		Workspace:           "./workspaces/queen",
		Provider:            "anthropic",
		MaxToolIterations:   25,
		RestrictToWorkspace: true,
		Channels:            []string{"telegram", "discord"},
		Env: map[string]string{
			"ANTHROPIC_API_KEY": "sk-test",
			"DEBUG":             "true",
		},
	}

	m := ClawConfigToMetadata(cc)
	if m["claw.workspace"] != "./workspaces/queen" {
		t.Errorf("workspace = %q, want './workspaces/queen'", m["claw.workspace"])
	}
	if m["claw.provider"] != "anthropic" {
		t.Errorf("provider = %q, want 'anthropic'", m["claw.provider"])
	}
	if m["claw.max_tool_iterations"] != "25" {
		t.Errorf("max_tool_iterations = %q, want '25'", m["claw.max_tool_iterations"])
	}
	if m["claw.restrict_to_workspace"] != "true" {
		t.Errorf("restrict_to_workspace = %q, want 'true'", m["claw.restrict_to_workspace"])
	}
	if m["claw.channels"] != "telegram,discord" {
		t.Errorf("channels = %q, want 'telegram,discord'", m["claw.channels"])
	}
	if m["claw.env.ANTHROPIC_API_KEY"] != "sk-test" {
		t.Errorf("env.ANTHROPIC_API_KEY = %q, want 'sk-test'", m["claw.env.ANTHROPIC_API_KEY"])
	}
	if m["claw.env.DEBUG"] != "true" {
		t.Errorf("env.DEBUG = %q, want 'true'", m["claw.env.DEBUG"])
	}
}

func TestClawConfigToMetadata_Empty(t *testing.T) {
	cc := &ClawAgentConfig{}
	m := ClawConfigToMetadata(cc)
	if len(m) != 0 {
		t.Errorf("ClawConfigToMetadata(empty) has %d entries, want 0", len(m))
	}
}

func TestLoadStartupConfig_WithClawConfig(t *testing.T) {
	data := `{
		"agents": [
			{
				"name": "queen",
				"role": "daemon",
				"cognitive_tier": "tactical",
				"runtime_type": "RUNTIME_CLAW",
				"runtime_image": "picoclaw",
				"system_prompt": "You are the Queen.",
				"claw_config": {
					"workspace": "./workspaces/queen",
					"provider": "anthropic",
					"max_tool_iterations": 25,
					"channels": ["telegram"],
					"env": {
						"ANTHROPIC_API_KEY": "sk-test"
					}
				}
			}
		]
	}`

	path := filepath.Join(t.TempDir(), "startup-claw.json")
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadStartupConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	a := cfg.Agents[0]
	if a.RuntimeType != "RUNTIME_CLAW" {
		t.Errorf("runtime_type = %q, want RUNTIME_CLAW", a.RuntimeType)
	}
	if a.SystemPrompt != "You are the Queen." {
		t.Errorf("system_prompt = %q, want 'You are the Queen.'", a.SystemPrompt)
	}
	if a.ClawConfig == nil {
		t.Fatal("claw_config should not be nil")
	}
	if a.ClawConfig.Workspace != "./workspaces/queen" {
		t.Errorf("claw_config.workspace = %q, want './workspaces/queen'", a.ClawConfig.Workspace)
	}
	if a.ClawConfig.MaxToolIterations != 25 {
		t.Errorf("claw_config.max_tool_iterations = %d, want 25", a.ClawConfig.MaxToolIterations)
	}
	if len(a.ClawConfig.Channels) != 1 || a.ClawConfig.Channels[0] != "telegram" {
		t.Errorf("claw_config.channels = %v, want [telegram]", a.ClawConfig.Channels)
	}
	if a.ClawConfig.Env["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Errorf("claw_config.env.ANTHROPIC_API_KEY = %q, want 'sk-test'", a.ClawConfig.Env["ANTHROPIC_API_KEY"])
	}
}
