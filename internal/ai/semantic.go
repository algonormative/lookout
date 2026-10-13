package ai

// GenAI semantic convention attribute keys.
// See: https://opentelemetry.io/docs/specs/semconv/gen-ai/
const (
	AttrGenAISystem        = "gen_ai.system"
	AttrGenAIRequestModel  = "gen_ai.request.model"
	AttrGenAIResponseModel = "gen_ai.response.model"
	AttrGenAIInputTokens   = "gen_ai.usage.input_tokens"
	AttrGenAIOutputTokens  = "gen_ai.usage.output_tokens"
	AttrGenAICostTotal     = "gen_ai.cost.total_cost" // LiteLLM
	AttrGenAIUsageCost     = "gen_ai.usage.cost"
	AttrGenAIToolName      = "gen_ai.tool.name"
	AttrGenAIToolCallID    = "gen_ai.tool.call_id"

	AttrAgentName      = "agent.name"
	AttrAgentTaskID    = "agent.task_id"
	AttrAgentSessionID = "agent.session_id"
	AttrAgentRepo      = "agent.repo"
	AttrAgentStepType  = "agent.step.type"

	AttrContainerID    = "container.id"
	AttrContainerImage = "container.image"
)

// Well-known span names.
const (
	SpanChatCompletion = "gen_ai.chat_completion"
	SpanToolCall       = "gen_ai.tool_call"
	SpanAgentSession   = "agent.session"
	SpanAgentStep      = "agent.step"
	SpanAgentSandbox   = "agent.sandbox"
)
