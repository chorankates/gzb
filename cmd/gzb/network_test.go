package main

import (
	"context"
	"testing"

	"github.com/conor/gzb/internal/ezsp"
)

type fakeJoinWindowConn struct {
	state      ezsp.NetworkStatus
	allowCalls int
	permit     []uint8
}

func (f *fakeJoinWindowConn) NetworkState(context.Context) (ezsp.NetworkStatus, error) {
	return f.state, nil
}
func (f *fakeJoinWindowConn) AllowJoins(context.Context) error {
	f.allowCalls++
	return nil
}
func (f *fakeJoinWindowConn) PermitJoining(_ context.Context, seconds uint8) error {
	f.permit = append(f.permit, seconds)
	return nil
}

func TestSetJoinWindowAppliesPolicyBeforeOpening(t *testing.T) {
	conn := &fakeJoinWindowConn{state: ezsp.NetworkJoined}
	if err := setJoinWindow(context.Background(), conn, 60); err != nil {
		t.Fatalf("setJoinWindow: %v", err)
	}
	if conn.allowCalls != 1 {
		t.Fatalf("AllowJoins calls = %d, want 1", conn.allowCalls)
	}
	if len(conn.permit) != 1 || conn.permit[0] != 60 {
		t.Fatalf("PermitJoining calls = %v, want [60]", conn.permit)
	}
}

func TestSetJoinWindowCanCloseWithoutEnablingPolicy(t *testing.T) {
	conn := &fakeJoinWindowConn{state: ezsp.NetworkJoined}
	if err := setJoinWindow(context.Background(), conn, 0); err != nil {
		t.Fatalf("setJoinWindow: %v", err)
	}
	if conn.allowCalls != 0 {
		t.Fatalf("AllowJoins calls = %d, want 0", conn.allowCalls)
	}
	if len(conn.permit) != 1 || conn.permit[0] != 0 {
		t.Fatalf("PermitJoining calls = %v, want [0]", conn.permit)
	}
}

func TestSetJoinWindowRejectsMissingNetwork(t *testing.T) {
	conn := &fakeJoinWindowConn{state: ezsp.NetworkNone}
	if err := setJoinWindow(context.Background(), conn, 60); err == nil {
		t.Fatal("setJoinWindow accepted an adapter without a network")
	}
	if conn.allowCalls != 0 || len(conn.permit) != 0 {
		t.Fatal("join configuration ran without a network")
	}
}
