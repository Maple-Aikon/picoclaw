import {
  parseToolCallsFromContent,
  parseToolCallsValue,
} from "@/features/chat/tool-calls"
import type { AssistantMessageKind, ChatMessage } from "@/store/chat"

type AssistantToolCalls = ChatMessage["toolCalls"]
type ExistingAssistantMessageState = Pick<
  ChatMessage,
  "kind" | "toolCalls" | "iterContext"
>

function parseIterContext(payload: Record<string, unknown>): string | undefined {
  if (typeof payload.iter_context !== "string") {
    return undefined
  }
  const iterContext = payload.iter_context.trim()
  return iterContext || undefined
}

export interface AssistantMessageCreateState {
  content: string
  kind: AssistantMessageKind
  toolCalls?: AssistantToolCalls
  iterContext?: string
}

export interface AssistantMessageUpdateState {
  content: string
  kind: AssistantMessageKind
  toolCalls?: AssistantToolCalls
  iterContext?: string
}

function normalizeAssistantMessageKind(
  payload: Record<string, unknown>,
): string | undefined {
  if (typeof payload.kind !== "string") {
    return undefined
  }
  const kind = payload.kind.trim().toLowerCase()
  return kind || undefined
}

function parseAssistantMessageKind(
  payload: Record<string, unknown>,
  toolCalls?: AssistantToolCalls,
): AssistantMessageKind {
  const kind = normalizeAssistantMessageKind(payload)
  if (kind === "thought") {
    return "thought"
  }
  if (kind === "tool_calls" || toolCalls) {
    return "tool_calls"
  }
  return "normal"
}

function hasExplicitAssistantKindPayload(
  payload: Record<string, unknown>,
): boolean {
  return (
    normalizeAssistantMessageKind(payload) !== undefined ||
    payload.tool_calls !== undefined
  )
}

function parseAssistantToolCalls(
  payload: Record<string, unknown>,
  content: string,
): AssistantToolCalls {
  return (
    parseToolCallsValue(payload.tool_calls) ??
    parseToolCallsFromContent(content)
  )
}

export function parseAssistantMessageCreateState(
  payload: Record<string, unknown>,
): AssistantMessageCreateState {
  const content = typeof payload.content === "string" ? payload.content : ""
  const toolCalls = parseAssistantToolCalls(payload, content)
  const iterContext = parseIterContext(payload)

  return {
    content,
    kind: parseAssistantMessageKind(payload, toolCalls),
    toolCalls,
    ...(iterContext ? { iterContext } : {}),
  }
}

export function parseAssistantMessageUpdateState(
  payload: Record<string, unknown>,
  existing?: ExistingAssistantMessageState,
): AssistantMessageUpdateState {
  const content = typeof payload.content === "string" ? payload.content : ""
  const toolCalls = parseAssistantToolCalls(payload, content)
  const iterContext = parseIterContext(payload) ?? existing?.iterContext

  if (hasExplicitAssistantKindPayload(payload)) {
    const kind = parseAssistantMessageKind(payload, toolCalls)
    return {
      content,
      kind,
      toolCalls: kind === "tool_calls" ? toolCalls : undefined,
      ...(iterContext ? { iterContext } : {}),
    }
  }

  if (toolCalls) {
    return {
      content,
      kind: "tool_calls",
      toolCalls,
      ...(iterContext ? { iterContext } : {}),
    }
  }

  if (existing?.kind === "thought" || existing?.kind === "tool_calls") {
    return {
      content,
      kind: "normal",
      toolCalls: undefined,
      ...(iterContext ? { iterContext } : {}),
    }
  }

  if (existing?.toolCalls) {
    return {
      content,
      kind: existing.kind ?? "normal",
      toolCalls: undefined,
      ...(iterContext ? { iterContext } : {}),
    }
  }

  return {
    content,
    kind: existing?.kind ?? "normal",
    ...(iterContext ? { iterContext } : {}),
  }
}
