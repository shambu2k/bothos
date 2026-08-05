# bothos PI adapter

Thin Node.js sidecar that runs a single dependency-upgrade through the
[PI Agent SDK](https://pi.dev/docs/latest/sdk). It is the first concrete
backend behind the Go `runtime.AgentRuntime` seam; the adapter protocol
(`internal/agent`) is language-agnostic so other runtimes can be added later.

## Protocol

- **stdin** (one JSON line): `{run_id, task, worktree, limits}`
- **stdout** (JSON-lines): `{type:"log"|"tool"|"intent"|"error", ...}`
- On success it emits one `intent` of kind `open_pr` (draft), constructed
  deterministically — the agent never chooses a branch or hands over a patch.

## Env

| Var | Required | Meaning |
|-----|----------|---------|
| `PI_MODEL` | no | model id (default `openrouter/deepseek/deepseek-v4-flash-0731`) |
| `OPENROUTER_API_KEY` | if using OpenRouter | OpenRouter auth (PI `ModelRuntime` resolves it) |
| `SEARCH_API_KEY` | no | if set, registers the optional `web_search` tool |
| `SEARCH_PROVIDER` | no | `tavily` (default) or `brave` |

`web_search` is **optional**: without `SEARCH_API_KEY` the tool is absent and
the agent completes the upgrade from pure LLM reasoning.

## Install

```sh
cd adapter/pi && npm install
```

## Local smoke test (no network-safe model)

The Go tests exercise the protocol with a fake adapter (see
`internal/agent/pi_runtime_test.go`). A real end-to-end run needs the secrets
above and a reachable model — see the Phase 2 plan's live-verify task.
