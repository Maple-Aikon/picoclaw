package protocoltypes

type ToolCall struct {
	ID               string         `json:"id"`
	Type             string         `json:"type,omitempty"`
	Function         *FunctionCall  `json:"function,omitempty"`
	Name             string         `json:"-"`
	Arguments        map[string]any `json:"-"`
	ThoughtSignature string         `json:"-"` // Internal use only
	ExtraContent     *ExtraContent  `json:"extra_content,omitempty"`
}

type ExtraContent struct {
	Google                  *GoogleExtra `json:"google,omitempty"`
	ToolFeedbackExplanation string       `json:"tool_feedback_explanation,omitempty"`
}

type GoogleExtra struct {
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

type FunctionCall struct {
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

type LLMResponse struct {
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall        `json:"tool_calls,omitempty"`
	FinishReason     string            `json:"finish_reason"`
	Usage            *UsageInfo        `json:"usage,omitempty"`
	Reasoning        string            `json:"reasoning"`
	ReasoningDetails []ReasoningDetail `json:"reasoning_details"`

	// Images are media items returned by the model (e.g. image generation
	// backends). Each entry carries a stable Ref resolved against a
	// MediaStore ("media://<id>") and the original data URL or remote URL.
	//
	// Ref is preferred when set (it survives serialization across
	// processes). URL is preserved verbatim from the provider response so
	// callers that bypass MediaStore (debug tooling, tests) still see the
	// source data:base64 URL.
	Images []ImageContent `json:"images,omitempty"`
}

type StreamChunk struct {
	Content          string
	ReasoningContent string

	// Images received in this SSE chunk. The provider's delta may include
	// image content independent of text (e.g. Gemini image generation).
	// The full set for a turn is accumulated in LLMResponse.Images by
	// parseStreamResponse; StreamChunk.Images lets streaming consumers
	// observe individual image events in real time.
	Images []ImageContent
}

// ImageContent represents a single image returned by the model.
//
// Ref is the canonical handle for MediaStore resolution ("media://<id>").
// URL is the original provider URL, typically "data:image/<fmt>;base64,<...>"
// for image generation backends (LiteLLM gemini-image-ai, OpenAI gpt-image-1).
// Index preserves the provider's ordering across streaming chunks.
type ImageContent struct {
	Type  string `json:"type,omitempty"` // "image_url" | "image"
	URL   string `json:"url,omitempty"`  // data:base64 or http(s)://
	Ref   string `json:"ref,omitempty"`  // media://<id> (after MediaStore.Store)
	Index int    `json:"index,omitempty"`
	Mime  string `json:"mime,omitempty"` // parsed from data URL "image/<fmt>" when available
}

type ReasoningDetail struct {
	Format string `json:"format"`
	Index  int    `json:"index"`
	Type   string `json:"type"`
	Text   string `json:"text"`
}

type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CacheControl marks a content block for LLM-side prefix caching.
// Currently only "ephemeral" is supported (used by Anthropic).
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// ContentBlock represents a structured segment of a system message.
// Adapters that understand SystemParts can use these blocks to set
// per-block cache control (e.g. Anthropic's cache_control: ephemeral).
type ContentBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	// Prompt metadata is internal to the agent runtime. It records which
	// structured prompt segment produced this block without changing provider
	// JSON.
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

type Attachment struct {
	Type        string `json:"type,omitempty"`
	Ref         string `json:"ref,omitempty"`
	URL         string `json:"url,omitempty"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

type Message struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ModelName        string         `json:"model_name,omitempty"`
	Media            []string       `json:"media,omitempty"`
	Attachments      []Attachment   `json:"attachments,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	SystemParts      []ContentBlock `json:"system_parts,omitempty"` // structured system blocks for cache-aware adapters
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`

	// Prompt metadata is internal to the agent runtime. It records where a
	// message or system part came from without changing provider/session JSON.
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

type ToolDefinition struct {
	Type     string                 `json:"type"`
	Function ToolFunctionDefinition `json:"function"`

	// Prompt metadata is internal to the agent runtime. Tool definitions are
	// model-visible capability prompts even though providers send them outside
	// the system message.
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

type ToolFunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
