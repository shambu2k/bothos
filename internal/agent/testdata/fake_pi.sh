#!/bin/sh
# Mimics `pi --mode rpc` well enough for deterministic Go unit tests (no LLM,
# no network). Reads JSONL commands from stdin, emits RPC events to STDOUT.
#
# Config (env):
#   FAKE_PI_PROMPT_FILE  -- append the stdin commands (prompt) here for asserts
#   FAKE_PI_EDIT         -- '1': create edited.txt in cwd after the prompt
#   FAKE_PI_NO_AGENT_END -- '1': omit agent_end (tests cancel/EOF paths)
#   FAKE_PI_IGNORE_TERM  -- '1': ignore SIGTERM (tests WaitDelay escalation)
#   FAKE_PI_REJECT       -- '1': respond to prompt with success:false
set -u
LOG="${FAKE_PI_PROMPT_FILE:-/tmp/fake_pi_in.jsonl}"

[ "${FAKE_PI_IGNORE_TERM:-}" = "1" ] && trap '' TERM

# Canned events go to STDOUT — the client reads these.
if [ "${FAKE_PI_REJECT:-}" = "1" ]; then
	echo '{"type":"response","command":"prompt","success":false}'
else
	echo '{"type":"response","command":"prompt","success":true}'
fi

if [ "${FAKE_PI_EDIT:-}" = "1" ]; then
	printf 'edited\n' > edited.txt
fi

if [ "${FAKE_PI_NO_AGENT_END:-}" != "1" ]; then
	echo '{"type":"agent_end"}'
fi

# RPC-style loop: read commands until stdin EOF (the prompt lands here).
while IFS= read -r line || [ -n "$line" ]; do
	echo "$line" >> "$LOG"
done
exit 0
