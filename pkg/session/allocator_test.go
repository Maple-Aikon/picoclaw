package session

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/routing"
)

func TestAllocateRouteSession_PerPeerDM(t *testing.T) {
	allocation := AllocateRouteSession(AllocationInput{
		AgentID:   "main",
		Channel:   "telegram",
		AccountID: "default",
		Peer: &routing.RoutePeer{
			Kind: "direct",
			ID:   "User123",
		},
		SessionPolicy: routing.SessionPolicy{
			DMScope: routing.DMScopePerPeer,
		},
	})

	if allocation.SessionKey != "agent:main:direct:user123" {
		t.Fatalf("SessionKey = %q, want %q", allocation.SessionKey, "agent:main:direct:user123")
	}
	if allocation.MainSessionKey != "agent:main:main" {
		t.Fatalf("MainSessionKey = %q, want %q", allocation.MainSessionKey, "agent:main:main")
	}
	if allocation.Scope.Version != ScopeVersionV1 {
		t.Fatalf("Scope.Version = %d, want %d", allocation.Scope.Version, ScopeVersionV1)
	}
	if len(allocation.Scope.Dimensions) != 1 || allocation.Scope.Dimensions[0] != "sender" {
		t.Fatalf("Scope.Dimensions = %v, want [sender]", allocation.Scope.Dimensions)
	}
	if allocation.Scope.Values["sender"] != "user123" {
		t.Fatalf("Scope.Values[sender] = %q, want user123", allocation.Scope.Values["sender"])
	}
}

func TestAllocateRouteSession_GroupPeer(t *testing.T) {
	allocation := AllocateRouteSession(AllocationInput{
		AgentID:   "main",
		Channel:   "slack",
		AccountID: "workspace-a",
		Peer: &routing.RoutePeer{
			Kind: "channel",
			ID:   "C001",
		},
		SessionPolicy: routing.SessionPolicy{
			DMScope: routing.DMScopePerAccountChannelPeer,
		},
	})

	if allocation.SessionKey != "agent:main:slack:channel:c001" {
		t.Fatalf("SessionKey = %q, want %q", allocation.SessionKey, "agent:main:slack:channel:c001")
	}
	if allocation.MainSessionKey != "agent:main:main" {
		t.Fatalf("MainSessionKey = %q, want %q", allocation.MainSessionKey, "agent:main:main")
	}
	if len(allocation.Scope.Dimensions) != 1 || allocation.Scope.Dimensions[0] != "chat" {
		t.Fatalf("Scope.Dimensions = %v, want [chat]", allocation.Scope.Dimensions)
	}
	if allocation.Scope.Values["chat"] != "channel:c001" {
		t.Fatalf("Scope.Values[chat] = %q, want channel:c001", allocation.Scope.Values["chat"])
	}
}
