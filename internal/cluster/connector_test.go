package cluster

import (
	"testing"
)

func TestConnector_ConnectAndGet(t *testing.T) {
	c := NewConnector()

	conn, err := c.Connect("vps2", "10.0.0.2:50051")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.NodeID != "vps2" {
		t.Errorf("NodeID=%q, want vps2", conn.NodeID)
	}
	if conn.State != ConnReady {
		t.Errorf("State=%s, want ready", conn.State)
	}

	got, err := c.GetConnection("vps2")
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.Address != "10.0.0.2:50051" {
		t.Errorf("Address=%q, want 10.0.0.2:50051", got.Address)
	}
}

func TestConnector_ConnectValidation(t *testing.T) {
	c := NewConnector()

	if _, err := c.Connect("", "addr"); err == nil {
		t.Error("empty node ID should fail")
	}
	if _, err := c.Connect("vps1", ""); err == nil {
		t.Error("empty address should fail")
	}
}

func TestConnector_Disconnect(t *testing.T) {
	c := NewConnector()
	c.Connect("vps2", "10.0.0.2:50051")

	if err := c.Disconnect("vps2"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if c.IsConnected("vps2") {
		t.Error("should not be connected after disconnect")
	}
	if c.ConnectionCount() != 0 {
		t.Errorf("ConnectionCount=%d, want 0", c.ConnectionCount())
	}

	if err := c.Disconnect("nonexistent"); err == nil {
		t.Error("disconnect nonexistent should fail")
	}
}

func TestConnector_IsConnected(t *testing.T) {
	c := NewConnector()

	if c.IsConnected("vps2") {
		t.Error("should not be connected before Connect")
	}

	c.Connect("vps2", "addr")
	if !c.IsConnected("vps2") {
		t.Error("should be connected after Connect")
	}
}

func TestConnector_ListConnections(t *testing.T) {
	c := NewConnector()
	c.Connect("vps1", "addr1")
	c.Connect("vps2", "addr2")
	c.Connect("vps3", "addr3")

	list := c.ListConnections()
	if len(list) != 3 {
		t.Errorf("len=%d, want 3", len(list))
	}
}

func TestConnector_ForwardMessage(t *testing.T) {
	c := NewConnector()
	c.Connect("vps2", "10.0.0.2:50051")

	msg := ForwardedMessage{
		ID:      "msg-001",
		FromPID: 100,
		ToPID:   200,
		Type:    "hello",
		Payload: []byte("hi from vps1"),
	}

	if err := c.ForwardMessage("vps2", msg); err != nil {
		t.Fatalf("ForwardMessage: %v", err)
	}

	conn, _ := c.GetConnection("vps2")
	if conn.MessagesSent != 1 {
		t.Errorf("MessagesSent=%d, want 1", conn.MessagesSent)
	}

	forwarded := c.ForwardedMessages()
	if len(forwarded) != 1 {
		t.Fatalf("forwarded len=%d, want 1", len(forwarded))
	}
	if forwarded[0].ID != "msg-001" {
		t.Errorf("forwarded[0].ID=%q, want msg-001", forwarded[0].ID)
	}
}

func TestConnector_ForwardMessage_NotConnected(t *testing.T) {
	c := NewConnector()

	msg := ForwardedMessage{ID: "msg-001", FromPID: 100, ToPID: 200}
	if err := c.ForwardMessage("vps2", msg); err == nil {
		t.Error("forward to nonexistent should fail")
	}
}

func TestConnector_ForwardMessage_UnhealthyConnection(t *testing.T) {
	c := NewConnector()
	c.Connect("vps2", "addr")

	// Make unhealthy.
	for i := 0; i < 3; i++ {
		c.RecordError("vps2")
	}

	conn, _ := c.GetConnection("vps2")
	if conn.State != ConnUnhealthy {
		t.Fatalf("State=%s, want unhealthy after 3 errors", conn.State)
	}

	msg := ForwardedMessage{ID: "msg-001"}
	if err := c.ForwardMessage("vps2", msg); err == nil {
		t.Error("forward to unhealthy connection should fail")
	}
}

func TestConnector_RecordError_Threshold(t *testing.T) {
	c := NewConnector()
	c.Connect("vps2", "addr")

	// 2 errors: still ready.
	c.RecordError("vps2")
	c.RecordError("vps2")

	conn, _ := c.GetConnection("vps2")
	if conn.State != ConnReady {
		t.Errorf("State=%s, want ready (only 2 errors)", conn.State)
	}

	// 3rd error: unhealthy.
	c.RecordError("vps2")
	conn, _ = c.GetConnection("vps2")
	if conn.State != ConnUnhealthy {
		t.Errorf("State=%s, want unhealthy after 3 errors", conn.State)
	}
}

func TestConnector_RecordSuccess_Recovery(t *testing.T) {
	c := NewConnector()
	c.Connect("vps2", "addr")

	// Make unhealthy.
	for i := 0; i < 3; i++ {
		c.RecordError("vps2")
	}

	// Recover.
	c.RecordSuccess("vps2")

	conn, _ := c.GetConnection("vps2")
	if conn.State != ConnReady {
		t.Errorf("State=%s, want ready after success", conn.State)
	}
	if conn.Errors != 0 {
		t.Errorf("Errors=%d, want 0 after success", conn.Errors)
	}
}

func TestConnector_ForwardMessage_ResetsErrors(t *testing.T) {
	c := NewConnector()
	c.Connect("vps2", "addr")

	c.RecordError("vps2")
	c.RecordError("vps2") // 2 errors, still ready

	msg := ForwardedMessage{ID: "msg-001"}
	c.ForwardMessage("vps2", msg)

	conn, _ := c.GetConnection("vps2")
	if conn.Errors != 0 {
		t.Errorf("Errors=%d, want 0 after successful forward", conn.Errors)
	}
}

func TestConnector_Reconnect(t *testing.T) {
	c := NewConnector()
	c.Connect("vps2", "old-addr")

	// Reconnect with new address.
	c.Connect("vps2", "new-addr")

	conn, _ := c.GetConnection("vps2")
	if conn.Address != "new-addr" {
		t.Errorf("Address=%q, want new-addr", conn.Address)
	}
	if conn.State != ConnReady {
		t.Errorf("State=%s, want ready after reconnect", conn.State)
	}
	if c.ConnectionCount() != 1 {
		t.Errorf("ConnectionCount=%d, want 1", c.ConnectionCount())
	}
}
