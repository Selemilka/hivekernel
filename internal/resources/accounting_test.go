package resources

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func setupAccountingTree(t *testing.T) (*process.Registry, *Accountant) {
	t.Helper()
	r := process.NewRegistry()

	r.Register(&process.Process{Name: "king", User: "root", VPS: "vps1", Role: process.RoleKernel})
	r.Register(&process.Process{PPID: 1, Name: "queen", User: "root", VPS: "vps1", Role: process.RoleDaemon})
	r.Register(&process.Process{PPID: 2, Name: "worker-alice", User: "alice", VPS: "vps1", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 2, Name: "worker-bob", User: "bob", VPS: "vps2", Role: process.RoleWorker})

	return r, NewAccountant(r)
}

func TestAccountantRecord(t *testing.T) {
	_, acc := setupAccountingTree(t)

	acc.Record(3, TierSonnet, 1000)
	acc.Record(3, TierSonnet, 500)

	if acc.RecordCount() != 2 {
		t.Fatalf("expected 2 records, got %d", acc.RecordCount())
	}
}

func TestAccountantUserUsage(t *testing.T) {
	_, acc := setupAccountingTree(t)

	acc.Record(3, TierSonnet, 1000) // alice, vps1
	acc.Record(4, TierSonnet, 2000) // bob, vps2
	acc.Record(3, TierOpus, 500)    // alice, vps1

	alice := acc.UserUsage("alice")
	if alice.TotalTokens != 1500 {
		t.Fatalf("alice total: expected 1500, got %d", alice.TotalTokens)
	}
	if alice.ByTier[TierSonnet] != 1000 {
		t.Fatalf("alice sonnet: expected 1000, got %d", alice.ByTier[TierSonnet])
	}
	if alice.ByTier[TierOpus] != 500 {
		t.Fatalf("alice opus: expected 500, got %d", alice.ByTier[TierOpus])
	}

	bob := acc.UserUsage("bob")
	if bob.TotalTokens != 2000 {
		t.Fatalf("bob total: expected 2000, got %d", bob.TotalTokens)
	}
}

func TestAccountantVPSUsage(t *testing.T) {
	_, acc := setupAccountingTree(t)

	acc.Record(3, TierSonnet, 1000) // alice, vps1
	acc.Record(4, TierSonnet, 2000) // bob, vps2
	acc.Record(1, TierOpus, 300)    // root, vps1

	vps1 := acc.VPSUsage("vps1")
	if vps1.TotalTokens != 1300 {
		t.Fatalf("vps1 total: expected 1300, got %d", vps1.TotalTokens)
	}

	vps2 := acc.VPSUsage("vps2")
	if vps2.TotalTokens != 2000 {
		t.Fatalf("vps2 total: expected 2000, got %d", vps2.TotalTokens)
	}
}

func TestAccountantProcessUsage(t *testing.T) {
	_, acc := setupAccountingTree(t)

	acc.Record(3, TierSonnet, 1000)
	acc.Record(3, TierSonnet, 500)
	acc.Record(3, TierOpus, 200)

	usage := acc.ProcessUsage(3)
	if usage.TotalTokens != 1700 {
		t.Fatalf("process 3 total: expected 1700, got %d", usage.TotalTokens)
	}
}

func TestAccountantTotalUsage(t *testing.T) {
	_, acc := setupAccountingTree(t)

	acc.Record(1, TierOpus, 100)
	acc.Record(3, TierSonnet, 1000)
	acc.Record(4, TierMini, 5000)

	total := acc.TotalUsage()
	if total.TotalTokens != 6100 {
		t.Fatalf("total: expected 6100, got %d", total.TotalTokens)
	}
	if total.ByTier[TierOpus] != 100 {
		t.Fatalf("opus: expected 100, got %d", total.ByTier[TierOpus])
	}
}
