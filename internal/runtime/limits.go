package runtime

// AgentWallSeconds is the maximum wall-clock time (seconds) a single agent
// run may take before the harness cancels it. Drafts are cheap to kill and
// large-backend clones (clone + install + tests + edits) routinely exceed
// 15m, so this is generous. Single source of truth for the worker pipelines;
// tune per-repo later via intent.Limits if needed.
const AgentWallSeconds int64 = 40 * 60
