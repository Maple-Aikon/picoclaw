//go:build bedrock

package bedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseResponse_CacheFields verifies that parseResponse populates the
// cache fields on UsageInfo from AWS Bedrock SDK TokenUsage
// (CacheReadInputTokens + CacheWriteInputTokens). When prompt caching hits,
// the SDK reports the cache hit/write token counts in those fields; this
// test exercises both populated and zero (no cache) cases.
func TestParseResponse_CacheFields(t *testing.T) {
	t.Run("with cache hit + cache write", func(t *testing.T) {
		output := &bedrockruntime.ConverseOutput{
			Output: &types.ConverseOutputMemberMessage{
				Value: types.Message{
					Role: types.ConversationRoleAssistant,
					Content: []types.ContentBlock{
						&types.ContentBlockMemberText{Value: "Cached reply."},
					},
				},
			},
			StopReason: types.StopReasonEndTurn,
			Usage: &types.TokenUsage{
				InputTokens:            aws.Int32(100),
				OutputTokens:           aws.Int32(20),
				TotalTokens:            aws.Int32(120),
				CacheReadInputTokens:   aws.Int32(80), // 80 of 100 input tokens served from cache
				CacheWriteInputTokens:  aws.Int32(20), // 20 fresh tokens written to cache
			},
		}

		resp, err := parseResponse(output)
		require.NoError(t, err)
		assert.Equal(t, 100, resp.Usage.PromptTokens)
		assert.Equal(t, 20, resp.Usage.CompletionTokens)
		assert.Equal(t, 120, resp.Usage.TotalTokens)
		assert.Equal(t, 80, resp.Usage.CacheReadInputTokens)
		assert.Equal(t, 20, resp.Usage.CacheWriteInputTokens)
	})

	t.Run("no cache (CacheRead + CacheWrite both nil)", func(t *testing.T) {
		output := &bedrockruntime.ConverseOutput{
			Output: &types.ConverseOutputMemberMessage{
				Value: types.Message{
					Role: types.ConversationRoleAssistant,
					Content: []types.ContentBlock{
						&types.ContentBlockMemberText{Value: "Fresh reply."},
					},
				},
			},
			StopReason: types.StopReasonEndTurn,
			Usage: &types.TokenUsage{
				InputTokens:  aws.Int32(10),
				OutputTokens: aws.Int32(5),
				// CacheReadInputTokens + CacheWriteInputTokens omitted
			},
		}

		resp, err := parseResponse(output)
		require.NoError(t, err)
		assert.Equal(t, 10, resp.Usage.PromptTokens)
		assert.Equal(t, 5, resp.Usage.CompletionTokens)
		assert.Equal(t, 0, resp.Usage.CacheReadInputTokens, "no cache → zero")
		assert.Equal(t, 0, resp.Usage.CacheWriteInputTokens, "no cache → zero")
	})

	t.Run("partial cache (only CacheRead set, CacheWrite nil)", func(t *testing.T) {
		output := &bedrockruntime.ConverseOutput{
			Output: &types.ConverseOutputMemberMessage{
				Value: types.Message{
					Role: types.ConversationRoleAssistant,
					Content: []types.ContentBlock{
						&types.ContentBlockMemberText{Value: "Hit."},
					},
				},
			},
			StopReason: types.StopReasonEndTurn,
			Usage: &types.TokenUsage{
				InputTokens:          aws.Int32(50),
				OutputTokens:         aws.Int32(10),
				CacheReadInputTokens: aws.Int32(40),
				// CacheWriteInputTokens omitted (cache read-only hit)
			},
		}

		resp, err := parseResponse(output)
		require.NoError(t, err)
		assert.Equal(t, 50, resp.Usage.PromptTokens)
		assert.Equal(t, 40, resp.Usage.CacheReadInputTokens)
		assert.Equal(t, 0, resp.Usage.CacheWriteInputTokens, "partial cache → CacheWrite=0")
	})
}

// TestParseStreamResponse_CacheFields verifies that the streaming path
// (ConverseStreamOutputMemberMetadata case in parseStreamResponse) also
// populates cache fields from AWS SDK TokenUsage.
func TestParseStreamResponse_CacheFields(t *testing.T) {
	t.Run("with cache hit + cache write", func(t *testing.T) {
		events := []types.ConverseStreamOutput{
			&types.ConverseStreamOutputMemberContentBlockDelta{
				Value: types.ContentBlockDeltaEvent{
					Delta:             &types.ContentBlockDeltaMemberText{Value: "Cached."},
					ContentBlockIndex: aws.Int32(0),
				},
			},
			&types.ConverseStreamOutputMemberMessageStop{
				Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn},
			},
			&types.ConverseStreamOutputMemberMetadata{
				Value: types.ConverseStreamMetadataEvent{
					Usage: &types.TokenUsage{
						InputTokens:           aws.Int32(100),
						OutputTokens:          aws.Int32(20),
						TotalTokens:           aws.Int32(120),
						CacheReadInputTokens:  aws.Int32(80),
						CacheWriteInputTokens: aws.Int32(20),
					},
				},
			},
		}

		stream := newMockStream(events)
		resp, err := parseStreamResponse(context.Background(), stream, nil)
		require.NoError(t, err)
		require.NotNil(t, resp.Usage)
		assert.Equal(t, 100, resp.Usage.PromptTokens)
		assert.Equal(t, 20, resp.Usage.CompletionTokens)
		assert.Equal(t, 80, resp.Usage.CacheReadInputTokens)
		assert.Equal(t, 20, resp.Usage.CacheWriteInputTokens)
	})

	t.Run("no cache (CacheRead + CacheWrite both nil)", func(t *testing.T) {
		events := []types.ConverseStreamOutput{
			&types.ConverseStreamOutputMemberContentBlockDelta{
				Value: types.ContentBlockDeltaEvent{
					Delta:             &types.ContentBlockDeltaMemberText{Value: "Fresh."},
					ContentBlockIndex: aws.Int32(0),
				},
			},
			&types.ConverseStreamOutputMemberMessageStop{
				Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn},
			},
			&types.ConverseStreamOutputMemberMetadata{
				Value: types.ConverseStreamMetadataEvent{
					Usage: &types.TokenUsage{
						InputTokens:  aws.Int32(10),
						OutputTokens: aws.Int32(5),
					},
				},
			},
		}

		stream := newMockStream(events)
		resp, err := parseStreamResponse(context.Background(), stream, nil)
		require.NoError(t, err)
		require.NotNil(t, resp.Usage)
		assert.Equal(t, 10, resp.Usage.PromptTokens)
		assert.Equal(t, 0, resp.Usage.CacheReadInputTokens, "no cache → zero")
		assert.Equal(t, 0, resp.Usage.CacheWriteInputTokens, "no cache → zero")
	})

	t.Run("partial cache (only CacheRead set)", func(t *testing.T) {
		events := []types.ConverseStreamOutput{
			&types.ConverseStreamOutputMemberContentBlockDelta{
				Value: types.ContentBlockDeltaEvent{
					Delta:             &types.ContentBlockDeltaMemberText{Value: "Hit."},
					ContentBlockIndex: aws.Int32(0),
				},
			},
			&types.ConverseStreamOutputMemberMessageStop{
				Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn},
			},
			&types.ConverseStreamOutputMemberMetadata{
				Value: types.ConverseStreamMetadataEvent{
					Usage: &types.TokenUsage{
						InputTokens:          aws.Int32(50),
						OutputTokens:         aws.Int32(10),
						CacheReadInputTokens: aws.Int32(40),
						// CacheWriteInputTokens omitted
					},
				},
			},
		}

		stream := newMockStream(events)
		resp, err := parseStreamResponse(context.Background(), stream, nil)
		require.NoError(t, err)
		require.NotNil(t, resp.Usage)
		assert.Equal(t, 50, resp.Usage.PromptTokens)
		assert.Equal(t, 40, resp.Usage.CacheReadInputTokens)
		assert.Equal(t, 0, resp.Usage.CacheWriteInputTokens, "partial cache → CacheWrite=0")
	})
}
