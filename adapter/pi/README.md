# bothos PI adapter

Thin Node.js sidecar for the [PI Agent SDK](https://pi.dev/docs/latest/sdk).
It is the concrete backend behind the Go `runtime.AgentRuntime` seam. The
adapter protocol (`internal/agent`) supports both dependency-remediation and
pull-request-review tasks; other runtime backends can be added independently.

## Protocol

- **stdin**: JSON-lines carrying runtime commands and task input.
- **stdout**: JSON-lines carrying logs, tool activity, results, and errors.
- **Upgrade runs**: PI writes a structured verdict and a draft-PR intent after
  it has made and committed a local change.
- **Review runs**: PI writes a structured review result; Bothos attaches
  deterministic findings and posts the validated result to GitHub.

The Go worker owns repository targeting, credentials, and GitHub writes. PI
never receives a write or comment token and does not push branches.

## Environment

| Variable | Required | Meaning |
|---|---|---|
| `PI_MODEL` | no | Model ID; defaults to `openrouter/deepseek/deepseek-v4-flash-0731`. |
| `OPENROUTER_API_KEY` | when using OpenRouter | OpenRouter authentication used by PI. |
| `SEARCH_API_KEY` | no | Enables PI's optional web-search tool. |
| `SEARCH_PROVIDER` | no | `tavily` (default) or `brave`. |

Without `SEARCH_API_KEY`, the adapter runs without web search.

## Install and test

```sh
cd adapter/pi
npm install
```

The Go test suite exercises the RPC protocol with a fake adapter. A live run
requires the configured model credentials and a running worker deployment.
