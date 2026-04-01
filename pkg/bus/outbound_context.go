package bus

import "strings"

// ContextFromLegacyOutbound builds a minimal outbound context from the legacy
// top-level outbound fields. This keeps older outbound publishers working
// while new publishers gradually start carrying the original InboundContext.
func ContextFromLegacyOutbound(msg OutboundMessage) InboundContext {
	return normalizeInboundContext(InboundContext{
		Channel:          strings.TrimSpace(msg.Channel),
		ChatID:           strings.TrimSpace(msg.ChatID),
		ReplyToMessageID: strings.TrimSpace(msg.ReplyToMessageID),
	})
}

// ContextFromLegacyOutboundMedia builds a minimal outbound context for media.
func ContextFromLegacyOutboundMedia(msg OutboundMediaMessage) InboundContext {
	return normalizeInboundContext(InboundContext{
		Channel: strings.TrimSpace(msg.Channel),
		ChatID:  strings.TrimSpace(msg.ChatID),
	})
}

// NormalizeOutboundMessage ensures Context is present and mirrors legacy
// top-level addressing fields from it so older senders keep working.
func NormalizeOutboundMessage(msg OutboundMessage) OutboundMessage {
	if msg.Context.isZero() {
		msg.Context = ContextFromLegacyOutbound(msg)
	} else {
		msg.Context = normalizeInboundContext(msg.Context)
	}

	if msg.Channel == "" {
		msg.Channel = msg.Context.Channel
	}
	if msg.ChatID == "" {
		msg.ChatID = msg.Context.ChatID
	}
	if msg.ReplyToMessageID == "" {
		msg.ReplyToMessageID = msg.Context.ReplyToMessageID
	}

	return msg
}

// NormalizeOutboundMediaMessage ensures media outbound messages also carry a
// normalized context while preserving the legacy top-level routing fields.
func NormalizeOutboundMediaMessage(msg OutboundMediaMessage) OutboundMediaMessage {
	if msg.Context.isZero() {
		msg.Context = ContextFromLegacyOutboundMedia(msg)
	} else {
		msg.Context = normalizeInboundContext(msg.Context)
	}

	if msg.Channel == "" {
		msg.Channel = msg.Context.Channel
	}
	if msg.ChatID == "" {
		msg.ChatID = msg.Context.ChatID
	}

	return msg
}
