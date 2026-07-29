// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"github.com/sipeed/picoclaw/pkg/agent/interfaces"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Pipeline holds the runtime dependencies used by Pipeline methods.
// It is constructed by runTurn via NewPipeline and passed to sub-methods
// so that the coordinator can delegate phase execution.
type Pipeline struct {
	Bus            interfaces.MessageBus
	Cfg            *config.Config
	ContextManager ContextManager
	Hooks          *HookManager
	Fallback       *providers.FallbackChain
	ChannelManager interfaces.ChannelManager
	MediaStore     media.MediaStore
	Steering       any // TODO: *Steering
	al             *AgentLoop

	// toolExec is the ExecuteTools dependency (Phase 12.28.1 Task 3 wiring).
	// Defaults to self (*Pipeline satisfies toolExecutor via ExecuteTools
	// method). Tests inject *fakeExecutor via SetToolExecutor to isolate
	// helper unit tests from full Pipeline.ExecuteTools side-effects.
	toolExec toolExecutor
}

// NewPipeline creates a Pipeline from an AgentLoop instance.
func NewPipeline(al *AgentLoop) *Pipeline {
	return &Pipeline{
		Bus:            al.bus,
		Cfg:            al.GetConfig(),
		ContextManager: al.contextManager,
		Hooks:          al.hooks,
		Fallback:       al.fallback,
		ChannelManager: al.channelManager,
		MediaStore:     al.mediaStore,
		Steering:       al.steering,
		al:             al,
		toolExec:       nil, // nil → lazy-self-binding via toolExecLazy
	}
}

// SetToolExecutor injects a custom toolExecutor (Phase 12.28.1 Task 3 — test
// seam for retryExecuteToolChain unit tests). Production code MUST NOT call
// this; default lazy-self-binding handles real wiring.
func (p *Pipeline) SetToolExecutor(te toolExecutor) { p.toolExec = te }

// toolExecLazy returns p.toolExec if set, otherwise p itself (self-binding).
// This pattern lets tests inject fakes without changing every call site; prod
// uses self-binding so no NewPipeline change is needed.
func (p *Pipeline) toolExecLazy() toolExecutor {
	if p.toolExec != nil {
		return p.toolExec
	}
	return p
}
