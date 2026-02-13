package cluster

import (
	"testing"
	"time"
)

func TestNodeRegistry_RegisterAndGet(t *testing.T) {
	reg := NewNodeRegistry()

	node := &NodeInfo{
		ID:       "vps1",
		Address:  "10.0.0.1:50051",
		Cores:    2,
		MemoryMB: 2048,
		DiskMB:   50000,
	}
	reg.Register(node)

	got, err := reg.Get("vps1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "vps1" {
		t.Errorf("got ID=%q, want vps1", got.ID)
	}
	if got.Status != NodeOnline {
		t.Errorf("got status=%s, want online", got.Status)
	}
	if got.LastHeartbeat.IsZero() {
		t.Error("LastHeartbeat should be set on register")
	}
	if reg.Count() != 1 {
		t.Errorf("Count=%d, want 1", reg.Count())
	}
}

func TestNodeRegistry_Deregister(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1", Address: "10.0.0.1:50051"})
	reg.Register(&NodeInfo{ID: "vps2", Address: "10.0.0.2:50051"})

	if err := reg.Deregister("vps1"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if reg.Count() != 1 {
		t.Errorf("Count=%d, want 1", reg.Count())
	}

	if err := reg.Deregister("vps99"); err == nil {
		t.Error("Deregister nonexistent should fail")
	}
}

func TestNodeRegistry_List(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1", Address: "a"})
	reg.Register(&NodeInfo{ID: "vps2", Address: "b"})
	reg.Register(&NodeInfo{ID: "vps3", Address: "c"})

	all := reg.List()
	if len(all) != 3 {
		t.Errorf("List len=%d, want 3", len(all))
	}
}

func TestNodeRegistry_ListOnline(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1"})
	reg.Register(&NodeInfo{ID: "vps2"})
	reg.Register(&NodeInfo{ID: "vps3"})

	_ = reg.SetStatus("vps2", NodeDraining)
	_ = reg.SetStatus("vps3", NodeOffline)

	online := reg.ListOnline()
	if len(online) != 1 {
		t.Errorf("ListOnline len=%d, want 1", len(online))
	}
	if online[0].ID != "vps1" {
		t.Errorf("expected vps1 online, got %s", online[0].ID)
	}
}

func TestNodeRegistry_UpdateHealth(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1", MemoryMB: 2048})

	err := reg.UpdateHealth("vps1", 1024, 10000, 5)
	if err != nil {
		t.Fatalf("UpdateHealth: %v", err)
	}

	node, _ := reg.Get("vps1")
	if node.MemoryUsedMB != 1024 {
		t.Errorf("MemoryUsedMB=%d, want 1024", node.MemoryUsedMB)
	}
	if node.ProcessCount != 5 {
		t.Errorf("ProcessCount=%d, want 5", node.ProcessCount)
	}
	if node.Status != NodeOnline {
		t.Errorf("Status=%s, want online (50%% memory)", node.Status)
	}
}

func TestNodeRegistry_UpdateHealth_Overloaded(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1", MemoryMB: 2048})

	// Push memory above 90%.
	_ = reg.UpdateHealth("vps1", 1900, 10000, 10)

	node, _ := reg.Get("vps1")
	if node.Status != NodeOverloaded {
		t.Errorf("Status=%s, want overloaded (93%% memory)", node.Status)
	}

	// Recovery: memory drops below 90%.
	_ = reg.UpdateHealth("vps1", 1000, 10000, 5)
	node, _ = reg.Get("vps1")
	if node.Status != NodeOnline {
		t.Errorf("Status=%s, want online after recovery", node.Status)
	}
}

func TestNodeRegistry_FindLeastLoaded(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1", MemoryMB: 2048, MemoryUsedMB: 1500, ProcessCount: 10})
	reg.Register(&NodeInfo{ID: "vps2", MemoryMB: 2048, MemoryUsedMB: 500, ProcessCount: 2})
	reg.Register(&NodeInfo{ID: "vps3", MemoryMB: 2048, MemoryUsedMB: 800, ProcessCount: 5})

	best, err := reg.FindLeastLoaded()
	if err != nil {
		t.Fatalf("FindLeastLoaded: %v", err)
	}
	if best.ID != "vps2" {
		t.Errorf("best=%s, want vps2 (least loaded)", best.ID)
	}
}

func TestNodeRegistry_FindLeastLoaded_Exclude(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1", MemoryMB: 2048, MemoryUsedMB: 1500})
	reg.Register(&NodeInfo{ID: "vps2", MemoryMB: 2048, MemoryUsedMB: 500})
	reg.Register(&NodeInfo{ID: "vps3", MemoryMB: 2048, MemoryUsedMB: 800})

	// Exclude vps2 (the best).
	best, err := reg.FindLeastLoaded("vps2")
	if err != nil {
		t.Fatalf("FindLeastLoaded: %v", err)
	}
	if best.ID != "vps3" {
		t.Errorf("best=%s, want vps3 (excluding vps2)", best.ID)
	}
}

func TestNodeRegistry_FindLeastLoaded_SkipOffline(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1", MemoryMB: 2048, MemoryUsedMB: 1500})
	reg.Register(&NodeInfo{ID: "vps2", MemoryMB: 2048, MemoryUsedMB: 100})
	_ = reg.SetStatus("vps2", NodeOffline) // best node is offline

	best, err := reg.FindLeastLoaded()
	if err != nil {
		t.Fatalf("FindLeastLoaded: %v", err)
	}
	if best.ID != "vps1" {
		t.Errorf("best=%s, want vps1 (vps2 is offline)", best.ID)
	}
}

func TestNodeRegistry_FindLeastLoaded_NoneAvailable(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1"})
	_ = reg.SetStatus("vps1", NodeOffline)

	_, err := reg.FindLeastLoaded()
	if err == nil {
		t.Error("FindLeastLoaded should fail when no online nodes")
	}
}

func TestNodeRegistry_CheckStale(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1"})
	reg.Register(&NodeInfo{ID: "vps2"})

	// Manually set vps1's heartbeat to the past.
	node, _ := reg.Get("vps1")
	node.LastHeartbeat = time.Now().Add(-2 * time.Minute)

	stale := reg.CheckStale(1 * time.Minute)
	if len(stale) != 1 || stale[0] != "vps1" {
		t.Errorf("stale=%v, want [vps1]", stale)
	}

	node, _ = reg.Get("vps1")
	if node.Status != NodeOffline {
		t.Errorf("vps1 status=%s, want offline", node.Status)
	}

	// vps2 should still be online.
	node2, _ := reg.Get("vps2")
	if node2.Status != NodeOnline {
		t.Errorf("vps2 status=%s, want online", node2.Status)
	}
}

func TestNodeInfo_MemoryFreePercent(t *testing.T) {
	node := &NodeInfo{MemoryMB: 2048, MemoryUsedMB: 1024}
	pct := node.MemoryFreePercent()
	if pct != 50.0 {
		t.Errorf("MemoryFreePercent=%f, want 50.0", pct)
	}
}

func TestNodeInfo_LoadScore(t *testing.T) {
	light := &NodeInfo{MemoryMB: 2048, MemoryUsedMB: 200, ProcessCount: 2}
	heavy := &NodeInfo{MemoryMB: 2048, MemoryUsedMB: 1800, ProcessCount: 20}

	if light.LoadScore() >= heavy.LoadScore() {
		t.Errorf("light score %f should be < heavy score %f",
			light.LoadScore(), heavy.LoadScore())
	}
}

func TestNodeRegistry_Heartbeat(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1"})

	// Set heartbeat to past.
	node, _ := reg.Get("vps1")
	old := node.LastHeartbeat

	time.Sleep(time.Millisecond)
	_ = reg.Heartbeat("vps1")

	node, _ = reg.Get("vps1")
	if !node.LastHeartbeat.After(old) {
		t.Error("Heartbeat should update timestamp")
	}

	if err := reg.Heartbeat("nonexistent"); err == nil {
		t.Error("Heartbeat on nonexistent node should fail")
	}
}

func TestNodeRegistry_SetStatus(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{ID: "vps1"})

	_ = reg.SetStatus("vps1", NodeDraining)
	node, _ := reg.Get("vps1")
	if node.Status != NodeDraining {
		t.Errorf("status=%s, want draining", node.Status)
	}

	if err := reg.SetStatus("nonexistent", NodeOnline); err == nil {
		t.Error("SetStatus on nonexistent should fail")
	}
}
