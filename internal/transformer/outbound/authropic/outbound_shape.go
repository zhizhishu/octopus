package authropic

// canonicalAnthropicRequestKeyOrder is the top-level member order that a genuine Claude CLI
// puts on the wire for POST /v1/messages. Octopus rebuilds every outbound Anthropic body in
// this order no matter how the downstream client ordered its own JSON.
//
// Why: replaying the client's own order looks correct when the client *is* the CLI, but a
// non-CLI client (SDK, curl, another gateway) sends its own order — frequently alphabetical
// (max_tokens, messages, model, ...). Replaying that leaks a non-CLI byte shape upstream and
// breaks the "outbound must be byte-identical to the real CLI" shape contract.
//
// Derived from archived golden captures; see
// `_artifacts/golden/_archive/samples/2026-10-10-*-golden-direct-v1_messages-beta-true-001.json`.
// Keep in sync with the CLI, not with any client.
var canonicalAnthropicRequestKeyOrder = []string{
	"model",
	"messages",
	"system",
	"tools",
	"metadata",
	"max_tokens",
	"thinking",
	"context_management",
	"safeguards",
	"output_config",
	"stream",
}
