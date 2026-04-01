package bus

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPublishConsume(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	ctx := context.Background()

	msg := InboundMessage{
		Channel:  "test",
		SenderID: "user1",
		ChatID:   "chat1",
		Content:  "hello",
	}

	if err := mb.PublishInbound(ctx, msg); err != nil {
		t.Fatalf("PublishInbound failed: %v", err)
	}

	got, ok := <-mb.InboundChan()
	if !ok {
		t.Fatal("ConsumeInbound returned ok=false")
	}
	if got.Content != "hello" {
		t.Fatalf("expected content 'hello', got %q", got.Content)
	}
	if got.Channel != "test" {
		t.Fatalf("expected channel 'test', got %q", got.Channel)
	}
	if got.Context.Channel != "test" {
		t.Fatalf("expected context channel 'test', got %q", got.Context.Channel)
	}
	if got.Context.ChatID != "chat1" {
		t.Fatalf("expected context chat ID 'chat1', got %q", got.Context.ChatID)
	}
	if got.Context.SenderID != "user1" {
		t.Fatalf("expected context sender ID 'user1', got %q", got.Context.SenderID)
	}
}

func TestPublishInbound_NormalizesLegacyFieldsIntoContext(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	msg := InboundMessage{
		Channel:   "slack",
		SenderID:  "U123",
		ChatID:    "C456/1712",
		Content:   "hello",
		MessageID: "1712.01",
		Peer:      Peer{Kind: "group", ID: "C456"},
		Metadata: map[string]string{
			"account_id":          "workspace-a",
			"team_id":             "T001",
			"reply_to_message_id": "1700.01",
			"is_mentioned":        "true",
			"parent_peer_kind":    "topic",
			"parent_peer_id":      "1712",
		},
	}

	if err := mb.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound failed: %v", err)
	}

	got := <-mb.InboundChan()
	if got.Context.Channel != "slack" {
		t.Fatalf("expected context channel slack, got %q", got.Context.Channel)
	}
	if got.Context.Account != "workspace-a" {
		t.Fatalf("expected context account workspace-a, got %q", got.Context.Account)
	}
	if got.Context.ChatType != "group" {
		t.Fatalf("expected context chat type group, got %q", got.Context.ChatType)
	}
	if got.Context.TopicID != "1712" {
		t.Fatalf("expected topic 1712, got %q", got.Context.TopicID)
	}
	if got.Context.SpaceType != "team" || got.Context.SpaceID != "T001" {
		t.Fatalf("expected team space T001, got %q/%q", got.Context.SpaceType, got.Context.SpaceID)
	}
	if !got.Context.Mentioned {
		t.Fatal("expected mentioned=true in context")
	}
	if got.Context.ReplyToMessageID != "1700.01" {
		t.Fatalf("expected reply_to_message_id 1700.01, got %q", got.Context.ReplyToMessageID)
	}
}

func TestPublishInbound_MirrorsContextIntoLegacyFields(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	msg := InboundMessage{
		Context: InboundContext{
			Channel:          "telegram",
			Account:          "bot-a",
			ChatID:           "-1001",
			ChatType:         "group",
			TopicID:          "42",
			SpaceID:          "guild-9",
			SpaceType:        "guild",
			SenderID:         "user-1",
			MessageID:        "777",
			Mentioned:        true,
			ReplyToMessageID: "666",
		},
		Content: "hi",
	}

	if err := mb.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound failed: %v", err)
	}

	got := <-mb.InboundChan()
	if got.Channel != "telegram" {
		t.Fatalf("expected legacy channel telegram, got %q", got.Channel)
	}
	if got.ChatID != "-1001" {
		t.Fatalf("expected legacy chat ID -1001, got %q", got.ChatID)
	}
	if got.SenderID != "user-1" {
		t.Fatalf("expected legacy sender ID user-1, got %q", got.SenderID)
	}
	if got.MessageID != "777" {
		t.Fatalf("expected legacy message ID 777, got %q", got.MessageID)
	}
	if got.Peer.Kind != "group" || got.Peer.ID != "-1001" {
		t.Fatalf("expected legacy peer group/-1001, got %q/%q", got.Peer.Kind, got.Peer.ID)
	}
	if got.Metadata["account_id"] != "bot-a" {
		t.Fatalf("expected mirrored account_id bot-a, got %q", got.Metadata["account_id"])
	}
	if got.Metadata["guild_id"] != "guild-9" {
		t.Fatalf("expected mirrored guild_id guild-9, got %q", got.Metadata["guild_id"])
	}
	if got.Metadata["parent_peer_kind"] != "topic" || got.Metadata["parent_peer_id"] != "42" {
		t.Fatalf(
			"expected mirrored topic parent peer, got %q/%q",
			got.Metadata["parent_peer_kind"],
			got.Metadata["parent_peer_id"],
		)
	}
	if got.Metadata["reply_to_message_id"] != "666" {
		t.Fatalf("expected mirrored reply_to_message_id 666, got %q", got.Metadata["reply_to_message_id"])
	}
	if got.Metadata["is_mentioned"] != "true" {
		t.Fatalf("expected mirrored is_mentioned true, got %q", got.Metadata["is_mentioned"])
	}
}

func TestPublishOutboundSubscribe(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	ctx := context.Background()

	msg := OutboundMessage{
		Channel: "telegram",
		ChatID:  "123",
		Content: "world",
	}

	if err := mb.PublishOutbound(ctx, msg); err != nil {
		t.Fatalf("PublishOutbound failed: %v", err)
	}

	got, ok := <-mb.OutboundChan()
	if !ok {
		t.Fatal("SubscribeOutbound returned ok=false")
	}
	if got.Content != "world" {
		t.Fatalf("expected content 'world', got %q", got.Content)
	}
}

func TestPublishOutbound_MirrorsContextToLegacyFields(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	msg := OutboundMessage{
		Context: InboundContext{
			Channel:          "telegram",
			ChatID:           "chat-42",
			ReplyToMessageID: "msg-9",
		},
		Content: "reply",
	}

	if err := mb.PublishOutbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishOutbound failed: %v", err)
	}

	got := <-mb.OutboundChan()
	if got.Channel != "telegram" {
		t.Fatalf("expected legacy channel telegram, got %q", got.Channel)
	}
	if got.ChatID != "chat-42" {
		t.Fatalf("expected legacy chat ID chat-42, got %q", got.ChatID)
	}
	if got.ReplyToMessageID != "msg-9" {
		t.Fatalf("expected mirrored reply_to_message_id msg-9, got %q", got.ReplyToMessageID)
	}
	if got.Context.Channel != "telegram" || got.Context.ChatID != "chat-42" {
		t.Fatalf("unexpected outbound context: %+v", got.Context)
	}
}

func TestPublishOutboundMedia_MirrorsContextToLegacyFields(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	msg := OutboundMediaMessage{
		Context: InboundContext{
			Channel: "slack",
			ChatID:  "C001",
		},
		Parts: []MediaPart{{Type: "image", Ref: "media://1"}},
	}

	if err := mb.PublishOutboundMedia(context.Background(), msg); err != nil {
		t.Fatalf("PublishOutboundMedia failed: %v", err)
	}

	got := <-mb.OutboundMediaChan()
	if got.Channel != "slack" {
		t.Fatalf("expected legacy channel slack, got %q", got.Channel)
	}
	if got.ChatID != "C001" {
		t.Fatalf("expected legacy chat ID C001, got %q", got.ChatID)
	}
	if got.Context.Channel != "slack" || got.Context.ChatID != "C001" {
		t.Fatalf("unexpected outbound media context: %+v", got.Context)
	}
}

func TestPublishInbound_ContextCancel(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	// Fill the buffer
	ctx := context.Background()
	for i := range defaultBusBufferSize {
		if err := mb.PublishInbound(ctx, InboundMessage{Content: "fill"}); err != nil {
			t.Fatalf("fill failed at %d: %v", i, err)
		}
	}

	// Now buffer is full; publish with a canceled context
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mb.PublishInbound(cancelCtx, InboundMessage{Content: "overflow"})
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestPublishInbound_BusClosed(t *testing.T) {
	mb := NewMessageBus()
	mb.Close()

	err := mb.PublishInbound(context.Background(), InboundMessage{Content: "test"})
	if err != ErrBusClosed {
		t.Fatalf("expected ErrBusClosed, got %v", err)
	}
}

func TestPublishOutbound_BusClosed(t *testing.T) {
	mb := NewMessageBus()
	mb.Close()

	err := mb.PublishOutbound(context.Background(), OutboundMessage{Content: "test"})
	if err != ErrBusClosed {
		t.Fatalf("expected ErrBusClosed, got %v", err)
	}
}

func TestConsumeInbound_ContextCancel(t *testing.T) {
	mb := NewMessageBus()

	defer mb.Close()

	for i := range defaultBusBufferSize {
		if err := mb.PublishInbound(context.Background(), InboundMessage{Content: "fill"}); err != nil {
			t.Fatalf("fill failed at %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	mb.PublishInbound(ctx, InboundMessage{Content: "ContextCancel"})

	select {
	case <-ctx.Done():
		t.Log("context canceled, as expected")

	case msg, ok := <-mb.InboundChan():
		if !ok {
			t.Fatal("expected ok=false when context is canceled")
		}
		if msg.Content == "ContextCancel" {
			t.Fatalf("expected content 'ContextCancel', got %q", msg.Content)
		}
	}
}

func TestConsumeInbound_BusClosed(t *testing.T) {
	mb := NewMessageBus()

	timer := time.AfterFunc(100*time.Millisecond, func() {
		mb.Close()
	})

	select {
	case <-timer.C:
		t.Log("context canceled, as expected")

	case _, ok := <-mb.InboundChan():
		if ok {
			t.Fatal("expected ok=false when context is canceled")
		}
	}
}

func TestSubscribeOutbound_BusClosed(t *testing.T) {
	mb := NewMessageBus()
	mb.Close()

	_, ok := <-mb.OutboundChan()
	if ok {
		t.Fatal("expected ok=false when bus is closed")
	}
}

func TestConcurrentPublishClose(t *testing.T) {
	mb := NewMessageBus()
	ctx := context.Background()

	const numGoroutines = 100
	var wg sync.WaitGroup
	wg.Add(numGoroutines + 1)

	// Spawn many goroutines trying to publish
	for range numGoroutines {
		go func() {
			defer wg.Done()
			// Use a short timeout context so we don't block forever after close
			publishCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			// Errors are expected; we just must not panic or deadlock
			_ = mb.PublishInbound(publishCtx, InboundMessage{Content: "concurrent"})
		}()
	}

	// Close from another goroutine
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		mb.Close()
	}()

	// Must complete without deadlock
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("test timed out - possible deadlock")
	}
}

func TestPublishInbound_FullBuffer(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	ctx := context.Background()

	// Fill the buffer
	for i := range defaultBusBufferSize {
		if err := mb.PublishInbound(ctx, InboundMessage{Content: "fill"}); err != nil {
			t.Fatalf("fill failed at %d: %v", i, err)
		}
	}

	// Buffer is full; publish with short timeout
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := mb.PublishInbound(timeoutCtx, InboundMessage{Content: "overflow"})
	if err == nil {
		t.Fatal("expected error when buffer is full and context times out")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	mb := NewMessageBus()

	// Multiple Close calls must not panic
	mb.Close()
	mb.Close()
	mb.Close()

	// After close, publish should return ErrBusClosed
	err := mb.PublishInbound(context.Background(), InboundMessage{Content: "test"})
	if err != ErrBusClosed {
		t.Fatalf("expected ErrBusClosed after multiple closes, got %v", err)
	}
}
