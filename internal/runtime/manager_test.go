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
