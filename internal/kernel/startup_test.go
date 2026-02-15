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
