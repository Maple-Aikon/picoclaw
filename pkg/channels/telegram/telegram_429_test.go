package telegram

// telegram_429_test.go — B8 / plan telegram-429-flood-classification-fix
// §11.5: regression tests for the 429 flood-control fix.
//
// WHY THE HELPER EXISTS (plan §11.1)
//
// The existing `stubCaller` can only return `(nil, error)`. telego
// then returns `internal execution: ...` — a plain error with NO
// `ta.Error` inside — so `errors.As(err, &apiErr)` would NOT match
// and a 429 could never be recognized.
//
// telego's `performRequest` (bot.go:175-176) does:
//
//	if !response.Ok {
//	    return fmt.Errorf("api: %w", response.Error)
//	}
//
// so returning a `*ta.Response` with `Ok: false` and an `Error`
// payload DOES put a `*ta.Error` in the error chain. That is what
// `errorResponse` builds.
//
// The tests below are written against the ACCEPTED behavior
// (plan §5, AC-1..AC-7):
//
//	T1   429 + retry_after=60   -> ErrRateLimit, retry_after visible,
//	                                EXACTLY ONE api call (no fallback)
//	T1b  429 without retry_after -> ErrRateLimit, retry_after == 0
//	T2   400 Bad Request        -> ErrSendFailed, TWO calls (fallback kept)
//	T3   500                    -> ErrTemporary
//	T3b  network error          -> ErrTemporary (pre-existing behavior)
//	T4   animator editFn 429    -> animator stops, no second call,
//	                                and Update() does NOT re-Record

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/media"
)

// errorResponse builds a ta.Response that telego turns into an
// `api: *ta.Error` chain (see performRequest, bot.go:175-176).
func errorResponse(errorCode int, description string, retryAfter int) *ta.Response {
	resp := &ta.Response{
		Ok: false,
		Error: &ta.Error{
			Description: description,
			ErrorCode:   errorCode,
		},
	}
	if retryAfter > 0 {
		resp.Error.Parameters = &ta.ResponseParameters{RetryAfter: retryAfter}
	}
	return resp
}

// newRateLimitChannel wires a channel whose API always returns the
// given response.
func newRateLimitChannel(t *testing.T, resp *ta.Response, calls *[]string) *TelegramChannel {
	t.Helper()
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			if calls != nil {
				*calls = append(*calls, url)
			}
			return resp, nil
		},
	}
	return newTestChannel(t, caller)
}

// --- T1: 429 with retry_after ------------------------------------------

func TestTelegramSend_429IsRateLimitAndDoesNotFallback(t *testing.T) {
	var urls []string
	ch := newRateLimitChannel(t,
		errorResponse(429, "Too Many Requests: retry after 60", 60), &urls)

	_, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: "hello",
	})
	require.Error(t, err)

	// AC-1: a 429 is a rate-limit error, not a temporary one.
	require.ErrorIs(t, err, channels.ErrRateLimit)
	require.NotErrorIs(t, err, channels.ErrTemporary)

	// AC-2: the retry_after survives the trip to the manager.
	require.Equal(t, 60*time.Second, channels.RateLimitRetryAfter(err))

	var rl *channels.RateLimitError
	require.True(t, errors.As(err, &rl), "errors.As must find the typed error")
	require.Equal(t, 60*time.Second, rl.RetryAfter)

	// AC-3 / T1: EXACTLY ONE api call. The plain-text fallback must
	// NOT fire — retrying a second API call while Telegram is
	// flood-blocking us is precisely what caused the 6-hour storm.
	require.Len(t, urls, 1, "a 429 must not trigger the fallback call: %v", urls)
	require.True(t, strings.Contains(urls[0], "sendMessage"), urls[0])
}

// --- T1b: 429 without retry_after --------------------------------------

func TestTelegramSend_429WithoutRetryAfter(t *testing.T) {
	var urls []string
	ch := newRateLimitChannel(t,
		errorResponse(429, "Too Many Requests", 0), &urls)

	_, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "hello"})
	require.Error(t, err)
	require.ErrorIs(t, err, channels.ErrRateLimit)
	require.Equal(t, time.Duration(0), channels.RateLimitRetryAfter(err))
	require.Len(t, urls, 1, "a 429 must not trigger the fallback call: %v", urls)
}

// --- T2: 400 Bad Request keeps the fallback ---------------------------

func TestTelegramSend_400KeepsPlainTextFallback(t *testing.T) {
	var urls []string
	ch := newRateLimitChannel(t,
		errorResponse(400, "Bad Request: can't parse entities", 0), &urls)

	_, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "hello"})
	require.Error(t, err)

	// A 400 is permanent, not a rate limit.
	require.ErrorIs(t, err, channels.ErrSendFailed)
	require.NotErrorIs(t, err, channels.ErrRateLimit)

	// T2: the HTML attempt + the plain-text fallback = TWO calls.
	// This is the PRE-EXISTING behavior and must not regress.
	require.Len(t, urls, 2, "the 400 fallback must be preserved: %v", urls)
}

// --- T3: 500 is temporary ----------------------------------------------

func TestTelegramSend_500IsTemporary(t *testing.T) {
	var urls []string
	ch := newRateLimitChannel(t,
		errorResponse(500, "Internal Server Error", 0), &urls)

	_, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "hello"})
	require.Error(t, err)
	require.ErrorIs(t, err, channels.ErrTemporary)
	require.NotErrorIs(t, err, channels.ErrRateLimit)
}

// --- T3b: a network error stays temporary -----------------------------

func TestTelegramSend_NetworkErrorStaysTemporary(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return nil, errors.New("send failed")
		},
	}
	ch := newTestChannel(t, caller)

	_, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "hello"})
	require.Error(t, err)
	require.ErrorIs(t, err, channels.ErrTemporary)
}

// --- T4: the animator stops on 429 -------------------------------------

func TestToolFeedbackAnimator_StopsOnRateLimit(t *testing.T) {
	var calls int
	anim := channels.NewToolFeedbackAnimator(
		func(ctx context.Context, chatID, messageID, content string) error {
			calls++
			return channels.NewRateLimitError(60*time.Second,
				errors.New("Too Many Requests"))
		})
	anim.Record("chat", "42", "working...")

	// The animator ticks every 3s. Wait long enough for several
	// ticks, then assert it stopped after the FIRST failure.
	time.Sleep(7 * time.Second)

	// Give any (buggy) extra tick a chance to land.
	time.Sleep(2 * time.Second)
	require.Equal(t, 1, calls,
		"the animator must stop calling editMessageText after a 429 (got %d calls)", calls)
}

// --- T4b: Update() must not restart the animator on 429 ---------------

func TestToolFeedbackAnimator_UpdateDoesNotRestartOnRateLimit(t *testing.T) {
	var calls int
	anim := channels.NewToolFeedbackAnimator(
		func(ctx context.Context, chatID, messageID, content string) error {
			calls++
			return channels.NewRateLimitError(60*time.Second,
				errors.New("Too Many Requests"))
		})
	anim.Record("chat", "42", "working...")

	// Let the first tick fail with a 429. The animator must stop after
	// that single call.
	time.Sleep(4 * time.Second)
	require.Equal(t, 1, calls,
		"the animator must stop after the first failed edit")

	// Update() itself performs ONE edit call. It must surface the error
	// but NOT re-Record: re-Record would start a fresh goroutine and
	// resume the 3-second loop.
	before := calls
	_, handled, err := anim.Update(context.Background(), "chat", "more")
	require.True(t, handled)
	require.Error(t, err)
	require.ErrorIs(t, err, channels.ErrRateLimit)
	require.Equal(t, before+1, calls,
		"Update() performs exactly one edit call")

	// If Update() had re-Recorded, the animation goroutine would be
	// running again and would add another call within the 3s tick.
	// No further call must appear.
	time.Sleep(5 * time.Second)
	require.Equal(t, before+1, calls,
		"Update() must not restart the animation goroutine after a 429")
}

// --- helper sanity: the response really carries ta.Error --------------

func TestErrorResponseCarriesTelegramAPIError(t *testing.T) {
	b, err := json.Marshal(&telego.Message{MessageID: 7})
	require.NoError(t, err)
	require.NotEmpty(t, b)

	resp := errorResponse(429, "Too Many Requests: retry after 60", 60)
	require.False(t, resp.Ok)
	require.Equal(t, 429, resp.Error.ErrorCode)
	require.Equal(t, 60, resp.Error.Parameters.RetryAfter)
}

// --- T5: SendMedia (single attachment) 429 keeps its classification ----
//
// Plan telegram-429-flood-classification-fix §17 (patch A2).
//
// BEFORE the patch, the media branch flattened every failure into
// `fmt.Errorf("telegram send media: %w", channels.ErrTemporary)`.
// A 429 therefore reached the Manager as ErrTemporary: the Manager
// retried it on a fixed exponential backoff (baseBackoff..maxBackoff)
// instead of waiting for the `retry_after` Telegram asked for, and it
// never stopped the tool-feedback animation loop (that loop only
// stops on errors.Is(err, ErrRateLimit)).
//
// AFTER the patch a 429 surfaces as ErrRateLimit carrying retry_after.

func TestTelegramSendMedia_429IsRateLimit(t *testing.T) {
	constructor := &multipartRecordingConstructor{}
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return errorResponse(429, "Too Many Requests: retry after 60", 60), nil
		},
	}
	ch := newTestChannelWithConstructor(t, caller, constructor)

	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "image.png")
	require.NoError(t, os.WriteFile(localPath, []byte("fake-png-content"), 0o644))

	ref, err := store.Store(localPath, media.MediaMeta{Filename: "image.png", ContentType: "image/png"}, "scope-1")
	require.NoError(t, err)

	_, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "12345",
		Parts:  []bus.MediaPart{{Type: "image", Ref: ref}},
	})

	require.Error(t, err)
	// The 429 must NOT be flattened into ErrTemporary: that is exactly
	// the defect this patch fixes.
	require.ErrorIs(t, err, channels.ErrRateLimit)
	require.NotErrorIs(t, err, channels.ErrTemporary)

	// The retry_after must survive to the Manager.
	require.Equal(t, 60*time.Second, channels.RateLimitRetryAfter(err))

	var rl *channels.RateLimitError
	require.True(t, errors.As(err, &rl), "errors.As must find the typed error")
	require.Equal(t, 60*time.Second, rl.RetryAfter)

	// Exactly ONE api call — a 429 must not trigger the
	// photo->document fallback.
	require.Len(t, caller.calls, 1, "a 429 must not trigger the document fallback: %v", caller.calls)
	require.Contains(t, caller.calls[0].URL, "sendPhoto")
}

// --- T6: SendMedia (media group) 429 keeps its classification ----------
//
// Plan telegram-429-flood-classification-fix §17 (patch A1).
//
// The media-group branch had the same flattening defect as the single
// attachment branch: `fmt.Errorf("telegram send media group: %w",
// channels.ErrTemporary)`.

func TestTelegramSendMediaGroup_429IsRateLimit(t *testing.T) {
	constructor := &multipartRecordingConstructor{}
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return errorResponse(429, "Too Many Requests: retry after 45", 45), nil
		},
	}
	ch := newTestChannelWithConstructor(t, caller, constructor)

	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)

	tmpDir := t.TempDir()
	firstPath := filepath.Join(tmpDir, "first.png")
	secondPath := filepath.Join(tmpDir, "second.png")
	require.NoError(t, os.WriteFile(firstPath, []byte("first-image"), 0o644))
	require.NoError(t, os.WriteFile(secondPath, []byte("second-image"), 0o644))

	firstRef, err := store.Store(firstPath, media.MediaMeta{Filename: "first.png", ContentType: "image/png"}, "scope-1")
	require.NoError(t, err)
	secondRef, err := store.Store(secondPath, media.MediaMeta{Filename: "second.png", ContentType: "image/png"}, "scope-1")
	require.NoError(t, err)

	_, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "12345",
		Parts: []bus.MediaPart{
			{Type: "image", Ref: firstRef},
			{Type: "image", Ref: secondRef},
		},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, channels.ErrRateLimit)
	require.NotErrorIs(t, err, channels.ErrTemporary)
	require.Equal(t, 45*time.Second, channels.RateLimitRetryAfter(err))

	// Exactly ONE api call — the group is not retried per-part.
	require.Len(t, caller.calls, 1, "a 429 must not retry the media group: %v", caller.calls)
	require.Contains(t, caller.calls[0].URL, "sendMediaGroup")
}

// --- T7: EditMessage fallback 429 keeps its classification -------------
//
// Plan telegram-429-flood-classification-fix §17 (patch B).
//
// The FIRST edit's own 429 was already classified (RC-4). But the
// plain-text FALLBACK call can itself be flood-blocked, and that 429
// escaped as an unknown error: the Manager treated it as a generic
// failure and fell through to a NEW SendMessage — adding outbound
// traffic at the exact moment Telegram was flood-blocking us.
//
// The fallback only fires on "Bad Request", so the stub answers:
//   call 1 -> 400 Bad Request   (parse failure, triggers the fallback)
//   call 2 -> 429               (the fallback is flood-blocked)

func TestTelegramEditMessage_Fallback429IsRateLimit(t *testing.T) {
	callCount := 0
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			callCount++
			if callCount == 1 {
				return errorResponse(400, "Bad Request: can't parse entities", 0), nil
			}
			return errorResponse(429, "Too Many Requests: retry after 30", 30), nil
		},
	}
	ch := newTestChannel(t, caller)

	err := ch.EditMessage(context.Background(), "12345", "42", "hello **world**")

	require.Error(t, err)
	require.ErrorIs(t, err, channels.ErrRateLimit)
	require.NotErrorIs(t, err, channels.ErrTemporary)
	require.Equal(t, 30*time.Second, channels.RateLimitRetryAfter(err))

	// The rich edit + the plain-text fallback = TWO calls, and no third.
	require.Len(t, caller.calls, 2, "edit attempt + fallback only: %v", caller.calls)
	require.Contains(t, caller.calls[0].URL, "editMessageText")
	require.Contains(t, caller.calls[1].URL, "editMessageText")
}
