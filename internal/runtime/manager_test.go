package runtime

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func TestNewManager(t *testing.T) {
	m := NewManager("localhost:50051", "python")
	if m.coreAddr != "localhost:50051" {
		t.Errorf("coreAddr = %q, want %q", m.coreAddr, "localhost:50051")
	}
	if m.pythonBin != "python" {
		t.Errorf("pythonBin = %q, want %q", m.pythonBin, "python")
	}
}

func TestNewManager_DefaultPython(t *testing.T) {
	m := NewManager("localhost:50051", "")
	if m.pythonBin != "python" {
		t.Errorf("pythonBin = %q, want %q", m.pythonBin, "python")
	}
}

func TestStartRuntime_VirtualProcess(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	proc := &process.Process{
		PID:  10,
		PPID: 1,
		Name: "virtual-agent",
		Role: process.RoleWorker,
	}

	rt, err := m.StartRuntime(proc, RuntimePython)
	if err != nil {
		t.Fatalf("StartRuntime: %v", err)
	}
	if rt.PID != 10 {
		t.Errorf("runtime PID = %d, want 10", rt.PID)
	}
	if rt.Cmd != nil {
		t.Error("virtual process should have nil Cmd")
	}
	if rt.Conn != nil {
		t.Error("virtual process should have nil Conn")
	}
	if proc.RuntimeAddr == "" {
		t.Error("RuntimeAddr should be set for virtual process")
	}

	// Verify GetRuntime returns it.
	got := m.GetRuntime(10)
	if got != rt {
		t.Error("GetRuntime should return the registered runtime")
	}
}

func TestStartRuntime_AlreadyRunning(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	proc := &process.Process{
		PID:  10,
		PPID: 1,
		Name: "agent-a",
	}

	_, err := m.StartRuntime(proc, RuntimePython)
	if err != nil {
		t.Fatalf("first StartRuntime: %v", err)
	}

	_, err = m.StartRuntime(proc, RuntimePython)
	if err == nil {
		t.Fatal("second StartRuntime should fail")
	}
}

func TestStopRuntime_Virtual(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	proc := &process.Process{
		PID:  10,
		PPID: 1,
		Name: "agent-v",
	}

	_, err := m.StartRuntime(proc, RuntimePython)
	if err != nil {
		t.Fatalf("StartRuntime: %v", err)
	}

	err = m.StopRuntime(10)
	if err != nil {
		t.Fatalf("StopRuntime: %v", err)
	}

	if m.GetRuntime(10) != nil {
		t.Error("runtime should be removed after stop")
	}
}

func TestStopRuntime_NotFound(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	err := m.StopRuntime(999)
	if err == nil {
		t.Fatal("StopRuntime for non-existent PID should fail")
	}
}

func TestListRuntimes(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	for i := uint64(10); i < 13; i++ {
		_, _ = m.StartRuntime(&process.Process{
			PID:  i,
			PPID: 1,
			Name: "agent",
		}, RuntimePython)
	}

	list := m.ListRuntimes()
	if len(list) != 3 {
		t.Errorf("ListRuntimes = %d, want 3", len(list))
	}
}

func TestGetClient_Virtual(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	proc := &process.Process{
		PID:  10,
		PPID: 1,
		Name: "agent-v",
	}
	_, _ = m.StartRuntime(proc, RuntimePython)

	// Virtual process has no Client.
	client := m.GetClient(10)
	if client != nil {
		t.Error("virtual process should have nil Client")
	}
}

func TestGetClient_NotFound(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	client := m.GetClient(999)
	if client != nil {
		t.Error("non-existent PID should return nil Client")
	}
}

func TestSetClawBin(t *testing.T) {
	m := NewManager("localhost:50051", "python")
	m.SetClawBin("/usr/local/bin/picoclaw")
	if m.clawBin != "/usr/local/bin/picoclaw" {
		t.Errorf("clawBin = %q, want %q", m.clawBin, "/usr/local/bin/picoclaw")
	}
}

func TestResolveClawBinary(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	tests := []struct {
		name         string
		clawBin      string
		runtimeImage string
		want         string
	}{
		{"default image with clawBin set", "/opt/picoclaw", "picoclaw", "/opt/picoclaw"},
		{"default image without clawBin", "", "picoclaw", "picoclaw"},
		{"empty image with clawBin", "/opt/picoclaw", "", "/opt/picoclaw"},
		{"explicit path overrides clawBin", "/opt/picoclaw", "/custom/path/claw", "/custom/path/claw"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m.clawBin = tt.clawBin
			got := m.resolveClawBinary(tt.runtimeImage)
			if got != tt.want {
				t.Errorf("resolveClawBinary(%q) = %q, want %q", tt.runtimeImage, got, tt.want)
			}
		})
	}
}

func TestSpawnReal_UnsupportedRuntime(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	proc := &process.Process{
		PID:         10,
		PPID:        1,
		Name:        "test-agent",
		RuntimeAddr: "some-image",
	}

	_, err := m.spawnReal(proc, RuntimeCustom)
	if err == nil {
		t.Fatal("spawnReal with unsupported runtime type should fail")
	}
	if !contains(err.Error(), "unsupported runtime type") {
		t.Errorf("error = %q, want to contain 'unsupported runtime type'", err)
	}
}

func TestSpawnReal_NoRuntimeImage(t *testing.T) {
	m := NewManager("localhost:50051", "python")

	proc := &process.Process{
		PID:  10,
		PPID: 1,
		Name: "test-agent",
		// RuntimeAddr is empty
	}

	_, err := m.spawnReal(proc, RuntimePython)
	if err == nil {
		t.Fatal("spawnReal with empty RuntimeAddr should fail")
	}
	if !contains(err.Error(), "no RuntimeImage") {
		t.Errorf("error = %q, want to contain 'no RuntimeImage'", err)
	}
}

func TestCogToProto(t *testing.T) {
	tests := []struct {
		in   process.CognitiveTier
		want string
	}{
		{process.CogStrategic, "COG_STRATEGIC"},
		{process.CogTactical, "COG_TACTICAL"},
		{process.CogOperational, "COG_OPERATIONAL"},
	}
	for _, tt := range tests {
		got := cogToProto(tt.in)
		if got.String() != tt.want {
			t.Errorf("cogToProto(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestLimitsToProto(t *testing.T) {
	limits := process.ResourceLimits{
		MaxTokensPerHour:   1000,
		MaxContextTokens:   2000,
		MaxChildren:        5,
		MaxConcurrentTasks: 3,
		TimeoutSeconds:     60,
		MaxTokensTotal:     50000,
	}
	pb := limitsToProto(limits)
	if pb.MaxTokensPerHour != 1000 {
		t.Errorf("MaxTokensPerHour = %d, want 1000", pb.MaxTokensPerHour)
	}
	if pb.MaxChildren != 5 {
		t.Errorf("MaxChildren = %d, want 5", pb.MaxChildren)
	}
	if pb.MaxTokensTotal != 50000 {
		t.Errorf("MaxTokensTotal = %d, want 50000", pb.MaxTokensTotal)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstr(s, substr)
}

func searchSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
