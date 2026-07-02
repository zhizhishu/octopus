package model

import (
	"encoding/json"
	"strconv"
	"strings"
)

// CodexDefaultSandbox is the sandbox value a genuine codex_exec reports in its
// turn-metadata for the common (default / workspace-write) case, packet-captured on
// the wire from codex_exec 0.142.5 on Linux. The --dangerously-bypass-approvals-and-sandbox
// mode reports "none", but the value a cloaked (non-codex) client should present is the
// ordinary sandboxed one.
const CodexDefaultSandbox = "seccomp"

// BuildCodexTurnMetadata serialises the codex turn-metadata JSON exactly as a genuine
// codex_exec client does: compact, with keys in the real serde struct order —
// installation_id, session_id, thread_id, turn_id, window_id, request_kind,
// thread_source, sandbox, turn_started_at_unix_ms. A Go map marshals keys
// alphabetically, a non-codex tell (the same reason BuildClaudeMetadataUserID hand-builds
// metadata.user_id). No "workspaces" key is emitted: real codex omits it entirely outside
// a git repo, and a cloaked (non-codex) client has no repo workspace, so the empty {} oct
// used to send matched neither the with-repo nor the without-repo real shape. thread_id
// mirrors session_id and window_id is sessionID+":0", exactly as the real client emits.
// Both the relay forward path and the channel/model-test path build turn-metadata through
// this one helper so a channel test is byte-shape-identical to real traffic.
func BuildCodexTurnMetadata(installationID, sessionID, turnID, sandbox string, turnStartedAtUnixMs int64) string {
	var b strings.Builder
	b.WriteString(`{"installation_id":`)
	b.WriteString(codexJSONString(installationID))
	b.WriteString(`,"session_id":`)
	b.WriteString(codexJSONString(sessionID))
	b.WriteString(`,"thread_id":`)
	b.WriteString(codexJSONString(sessionID))
	b.WriteString(`,"turn_id":`)
	b.WriteString(codexJSONString(turnID))
	b.WriteString(`,"window_id":`)
	b.WriteString(codexJSONString(sessionID + ":0"))
	b.WriteString(`,"request_kind":"turn","thread_source":"user","sandbox":`)
	b.WriteString(codexJSONString(sandbox))
	b.WriteString(`,"turn_started_at_unix_ms":`)
	b.WriteString(strconv.FormatInt(turnStartedAtUnixMs, 10))
	b.WriteString("}")
	return b.String()
}

// codexJSONString encodes a string as a JSON string literal (quoted + escaped) so the
// hand-built object stays valid JSON even if a value ever contains a quote/backslash.
func codexJSONString(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(out)
}
