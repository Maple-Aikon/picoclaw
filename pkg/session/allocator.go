package session

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/routing"
)

// Allocation contains the concrete session keys selected for a routed turn.
// The current implementation intentionally preserves the legacy session-key
// layout while moving key construction out of the router.
type Allocation struct {
	Scope          SessionScope
	SessionKey     string
	MainSessionKey string
}

// AllocationInput contains the routing result and peer context needed to
// derive the session keys for a turn.
type AllocationInput struct {
	AgentID       string
	Channel       string
	AccountID     string
	Peer          *routing.RoutePeer
	SessionPolicy routing.SessionPolicy
}

// AllocateRouteSession maps a route decision onto the current legacy
// agent-scoped session-key format.
func AllocateRouteSession(input AllocationInput) Allocation {
	scope := buildSessionScope(input)
	sessionKey := strings.ToLower(routing.BuildAgentPeerSessionKey(routing.SessionKeyParams{
		AgentID:       input.AgentID,
		Channel:       input.Channel,
		AccountID:     input.AccountID,
		Peer:          input.Peer,
		DMScope:       input.SessionPolicy.DMScope,
		IdentityLinks: input.SessionPolicy.IdentityLinks,
	}))
	mainSessionKey := strings.ToLower(routing.BuildAgentMainSessionKey(input.AgentID))
	return Allocation{
		Scope:          scope,
		SessionKey:     sessionKey,
		MainSessionKey: mainSessionKey,
	}
}

func buildSessionScope(input AllocationInput) SessionScope {
	scope := SessionScope{
		Version: ScopeVersionV1,
		AgentID: routing.NormalizeAgentID(input.AgentID),
		Channel: strings.ToLower(strings.TrimSpace(input.Channel)),
		Account: routing.NormalizeAccountID(input.AccountID),
	}

	peer := input.Peer
	if peer == nil {
		peer = &routing.RoutePeer{Kind: "direct"}
	}

	peerKind := strings.ToLower(strings.TrimSpace(peer.Kind))
	if peerKind == "" {
		peerKind = "direct"
	}

	switch peerKind {
	case "direct":
		if input.SessionPolicy.DMScope == routing.DMScopeMain {
			return scope
		}
		peerID := routing.CanonicalSessionPeerID(
			input.Channel,
			peer.ID,
			input.SessionPolicy.DMScope,
			input.SessionPolicy.IdentityLinks,
		)
		if peerID == "" {
			return scope
		}
		scope.Dimensions = []string{"sender"}
		scope.Values = map[string]string{
			"sender": peerID,
		}
	default:
		peerID := strings.ToLower(strings.TrimSpace(peer.ID))
		if peerID == "" {
			peerID = "unknown"
		}
		scope.Dimensions = []string{"chat"}
		scope.Values = map[string]string{
			"chat": fmt.Sprintf("%s:%s", peerKind, peerID),
		}
	}

	return scope
}
