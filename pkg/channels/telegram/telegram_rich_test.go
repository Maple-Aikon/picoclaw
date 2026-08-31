package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

// Phase 12.64 — Telegram Rich Markdown (use_markdown_v2 = rich mode).
// T1-T9 TDD tests: rich path must use sendRichMessage / sendRichMessageDraft /
// EditMessageTextParams.RichMessage with pass-through markdown (no convert,
// no escape), while non-rich keeps HTML behavior.

func richBody(t *testing.T, call stubCall) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(call.Data.BodyRaw, &m))
	return m
}

func richMessageOf(t *testing.T, call stubCall) map[string]any {
	t.Helper()
	rm, ok := richBody(t, call)["rich_message"].(map[string]any)
	require.True(t, ok, "body must contain rich_message object")
	return rm
}

// T1 — Send rich: exactly one sendRichMessage call, rich_message.markdown is
// the original markdown (no convert/escape), no parse_mode in body.
func TestSendRich_ShortMessage_SingleRichCall(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg.UseMarkdownV2 = true

	content := "# Hello **world**\n\n- item one\n- item two"
	_, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: content,
	})

	assert.NoError(t, err)
	require.Len(t, caller.calls, 1, "rich short message must be exactly one API call")
	assert.Contains(t, caller.calls[0].URL, "sendRichMessage")
	rm := richMessageOf(t, caller.calls[0])
	assert.Equal(t, normalizeRichMarkdown(content), rm["markdown"], "rich_message.markdown must match normalized rich markdown")
	_, hasParse := richBody(t, caller.calls[0])["parse_mode"]
	assert.False(t, hasParse, "rich send must not set parse_mode")
}

// T2a — Rich fallback (parse-invalid): sendRichMessage fails → second call is
// sendMessage plain text (no parse mode), content = original markdown.
func TestSendRich_Fallback_ParseInvalid(t *testing.T) {
	callCount := 0
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			callCount++
			if callCount == 1 {
				return nil, errors.New("Bad Request: can't parse rich markdown")
			}
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg.UseMarkdownV2 = true

	content := "Hello **world**"
	_, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: content,
	})

	assert.NoError(t, err)
	require.Len(t, caller.calls, 2, "rich attempt + plain text fallback")
	assert.Contains(t, caller.calls[0].URL, "sendRichMessage")
	assert.Contains(t, caller.calls[1].URL, "sendMessage")
	body := richBody(t, caller.calls[1])
	assert.Equal(t, content, body["text"], "fallback must send original markdown as plain text")
	_, hasParse := body["parse_mode"]
	assert.False(t, hasParse, "fallback must not set parse_mode")
}

// T2b — Rich fallback (method/network error): same plain fallback, never
// surfaces ErrTemporary.
func TestSendRich_Fallback_MethodError(t *testing.T) {
	callCount := 0
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			callCount++
			if callCount == 1 {
				return nil, errors.New("method not found")
			}
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg.UseMarkdownV2 = true

	_, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: "Hello **world**",
	})

	assert.NoError(t, err)
	require.Len(t, caller.calls, 2)
	assert.Contains(t, caller.calls[0].URL, "sendRichMessage")
	assert.Contains(t, caller.calls[1].URL, "sendMessage")
}

// T2c — Rich fallback both fail: wraps ErrTemporary like the HTML path.
func TestSendRich_Fallback_BothFail(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return nil, errors.New("send failed")
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg.UseMarkdownV2 = true

	_, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: "Hello",
	})

	assert.Error(t, err)
	assert.True(t, errors.Is(err, channels.ErrTemporary), "error should wrap ErrTemporary")
	assert.Equal(t, 2, len(caller.calls))
}

// T3 — EditMessage rich: editMessageText body carries rich_message.markdown
// and NO parse_mode (DOUBT-F6: no conflict in payload).
func TestEditMessage_Rich(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg.UseMarkdownV2 = true

	content := "# Updated **content**"
	err := ch.EditMessage(context.Background(), "12345", "42", content)

	assert.NoError(t, err)
	require.Len(t, caller.calls, 1)
	assert.Contains(t, caller.calls[0].URL, "editMessageText")
	body := richBody(t, caller.calls[0])
	rm, ok := body["rich_message"].(map[string]any)
	require.True(t, ok, "rich edit must carry rich_message")
	assert.Equal(t, content, rm["markdown"])
	_, hasParse := body["parse_mode"]
	assert.False(t, hasParse, "rich edit must not set parse_mode")
}

// T3b — EditMessage rich fallback: Bad Request → plain edit (no rich_message,
// no parse_mode).
func TestEditMessage_Rich_Fallback_BadRequest(t *testing.T) {
	callCount := 0
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			callCount++
			if callCount == 1 {
				return nil, errors.New("Bad Request: can't parse entities")
			}
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg.UseMarkdownV2 = true

	content := "# Updated **content**"
	err := ch.EditMessage(context.Background(), "12345", "42", content)

	assert.NoError(t, err)
	require.Len(t, caller.calls, 2, "rich edit attempt + plain fallback")
	body := richBody(t, caller.calls[1])
	assert.Equal(t, content, body["text"])
	_, hasRich := body["rich_message"]
	assert.False(t, hasRich, "fallback edit must not carry rich_message")
	_, hasParse := body["parse_mode"]
	assert.False(t, hasParse, "fallback edit must not set parse_mode")
}

// T4 — sendCaptionText rich: caption >1024 split-off text goes through
// sendRichMessage (not HTML).
func TestSendMedia_LongCaption_RichTextChunk(t *testing.T) {
	constructor := &multipartRecordingConstructor{}
	longCaption := strings.Repeat("a", telegramCaptionLimit) + " tail overflow"
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			println("URL:", url)
			switch {
			case strings.Contains(url, "sendRichMessage"):
				return successResponseWithMessageID(t, 201), nil
			case strings.Contains(url, "sendPhoto"):
				return successResponseWithMessageID(t, 202), nil
			default:
				t.Fatalf("unexpected API call: %s", url)
				return nil, nil
			}
		},
	}
	ch := newTestChannelWithConstructor(t, caller, constructor)
	ch.tgCfg.UseMarkdownV2 = true

	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "image.png")
	require.NoError(t, os.WriteFile(path, []byte("img"), 0o644))
	ref, err := store.Store(path, media.MediaMeta{Filename: "image.png", ContentType: "image/png"}, "scope-1")
	require.NoError(t, err)

	ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "12345",
		Parts: []bus.MediaPart{{
			Type:    "image",
			Ref:     ref,
			Caption: longCaption,
		}},
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"201", "202"}, ids)
	require.Len(t, caller.calls, 2)
	assert.Contains(t, caller.calls[0].URL, "sendRichMessage")
	assert.Contains(t, caller.calls[1].URL, "sendPhoto")
	assert.Equal(t, "", constructor.calls[0].Parameters["caption"])
}

// T5 — Media caption ≤1024 stays RAW in BOTH modes (Q3): no parse_mode param
// on the photo, caption text untouched.
func TestSendMedia_CaptionRaw_BothModes(t *testing.T) {
	caption := "Simple **raw** caption #1"
	for _, tc := range []struct {
		name string
		rich bool
	}{
		{name: "html_mode", rich: false},
		{name: "rich_mode", rich: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			constructor := &multipartRecordingConstructor{}
			caller := &stubCaller{
				callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
					if strings.Contains(url, "sendPhoto") {
						return successResponseWithMessageID(t, 202), nil
					}
					t.Fatalf("unexpected API call: %s", url)
					return nil, nil
				},
			}
			ch := newTestChannelWithConstructor(t, caller, constructor)
			ch.tgCfg.UseMarkdownV2 = tc.rich

			store := media.NewFileMediaStore()
			ch.SetMediaStore(store)
			tmpDir := t.TempDir()
			path := filepath.Join(tmpDir, "image.png")
			require.NoError(t, os.WriteFile(path, []byte("img"), 0o644))
			ref, err := store.Store(path, media.MediaMeta{Filename: "image.png", ContentType: "image/png"}, "scope-1")
			require.NoError(t, err)

			_, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
				ChatID: "12345",
				Parts: []bus.MediaPart{{
					Type:    "image",
					Ref:     ref,
					Caption: caption,
				}},
			})

			require.NoError(t, err)
			require.Len(t, constructor.calls, 1)
			assert.Equal(t, caption, constructor.calls[0].Parameters["caption"], "caption must stay raw")
			_, hasParse := constructor.calls[0].Parameters["parse_mode"]
			assert.False(t, hasParse, "media caption must never set parse_mode")
		})
	}
}

// T6 — Streaming rich: Update pushes sendRichMessageDraft with a stable
// DraftID; Finalize sends one sendRichMessage with the full content.
func TestStreaming_Rich_UsesRichDraft(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			switch {
			case strings.Contains(url, "sendRichMessageDraft"):
				return &ta.Response{Ok: true, Result: []byte("true")}, nil
			case strings.Contains(url, "sendRichMessage"):
				return successResponse(t), nil
			case strings.Contains(url, "sendMessageDraft"):
				return &ta.Response{Ok: true, Result: []byte("true")}, nil
			default:
				t.Fatalf("unexpected API call: %s", url)
				return nil, nil
			}
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg = &config.TelegramSettings{
		UseMarkdownV2: true,
		Streaming:     config.StreamingConfig{Enabled: true},
	}

	s, err := ch.BeginStream(context.Background(), "12345")
	require.NoError(t, err)
	st := s.(*telegramStreamer)
	st.buffer = nil // bypass draft buffer for deterministic call counts

	content1 := strings.Repeat("a", 200)
	content2 := strings.Repeat("b", 400)
	require.NoError(t, s.Update(context.Background(), content1))
	require.NoError(t, s.Update(context.Background(), content2))
	require.NoError(t, s.Finalize(context.Background(), content2))

	require.GreaterOrEqual(t, len(caller.calls), 3)
	assert.Contains(t, caller.calls[0].URL, "sendRichMessageDraft", "Update must use rich draft")
	rm := richMessageOf(t, caller.calls[0])
	assert.Equal(t, content1, rm["markdown"], "draft markdown must be original content")
	draftID1 := richBody(t, caller.calls[0])["draft_id"]
	assert.Contains(t, caller.calls[1].URL, "sendRichMessageDraft")
	assert.Equal(t, draftID1, richBody(t, caller.calls[1])["draft_id"], "DraftID must stay stable across updates")
	assert.Contains(t, caller.calls[2].URL, "sendRichMessage", "Finalize must send one rich message")
}

// T7 — fitToolFeedbackForTelegram rich: identity when ≤ limit (no expand).
func TestFitToolFeedback_Rich_Identity(t *testing.T) {
	short := "🔧 `read_file` — **bold** stays as-is"
	got := fitToolFeedbackForTelegram(short, formatModeRich, 4096)
	assert.Equal(t, short, got, "rich mode must return content unchanged when within limit")

	long := strings.Repeat("x", 5000)
	fitted := fitToolFeedbackForTelegram(long, formatModeRich, 4096)
	assert.LessOrEqual(t, len([]rune(fitted)), 4096, "over-limit content must be trimmed")
}

// T9a — Q1: NewTelegramChannel sets MaxMessageLength 30000 when rich, 4000 otherwise.
func TestNewTelegramChannel_RichChunkLimit(t *testing.T) {
	richCh, err := NewTelegramChannel(
		&config.Channel{Type: config.ChannelTelegram, Enabled: true},
		&config.TelegramSettings{Token: *config.NewSecureString(testToken), UseMarkdownV2: true},
		bus.NewMessageBus(),
	)
	require.NoError(t, err)
	assert.Equal(t, 30000, richCh.MaxMessageLength(), "rich mode must allow 30000-char chunks")

	htmlCh, err := NewTelegramChannel(
		&config.Channel{Type: config.ChannelTelegram, Enabled: true},
		&config.TelegramSettings{Token: *config.NewSecureString(testToken)},
		bus.NewMessageBus(),
	)
	require.NoError(t, err)
	assert.Equal(t, 4000, htmlCh.MaxMessageLength(), "HTML mode keeps legacy 4000 limit")
}

// T9b — Q1: rich Send with ~25k content is ONE call (no 4096 re-split).
func TestSendRich_LongContent_SingleCall(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg.UseMarkdownV2 = true

	longContent := strings.Repeat("m", 25000)
	_, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: longContent,
	})

	assert.NoError(t, err)
	require.Len(t, caller.calls, 1, "25k rich message must be a single sendRichMessage call")
	assert.Contains(t, caller.calls[0].URL, "sendRichMessage")
}
