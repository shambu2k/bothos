#!/bin/sh
# Mimics `pi --mode rpc` well enough for deterministic Go unit tests (no LLM,
# no network). Reads JSONL commands from stdin, emits RPC events to STDOUT.
# Supports multiple prompts on one stdin (settle -> verdict -> feedback rounds).
#
# Config (env):
#   FAKE_PI_PROMPT_FILE      -- append the stdin commands (prompt) here for asserts
#   FAKE_PI_EDIT             -- '1': on the first prompt, create a branch and a
#                               commit in cwd so the worktree is ahead of
#                               origin/HEAD (the harness's commits gate)
#   FAKE_PI_VERDICT          -- JSON string; written to .bothos/verdict.json on
#                               prompt number $FAKE_PI_VERDICT_ON_PROMPT (default 1)
#   FAKE_PI_VERDICT_ON_PROMPT-- prompt number on which to write the verdict
#   FAKE_PI_REVIEW           -- JSON string written to .bothos/review.json on
#                               the first prompt
#   FAKE_PI_NO_AGENT_END     -- '1': omit the settle events (cancel/EOF paths)
#   FAKE_PI_AGENT_END        -- '1': emit agent_end (willRetry:false) instead of
#                               agent_settled, exercising the old-build fallback
#   FAKE_PI_IGNORE_TERM      -- '1': ignore SIGTERM (tests WaitDelay escalation)
#   FAKE_PI_REJECT           -- '1': respond to prompt with success:false
#
# Each turn ends with exactly ONE settle signal (agent_settled by default, or
# agent_end when FAKE_PI_AGENT_END is set). A single settle per turn keeps the
# verdict loop unambiguous: emitting two equivalent settle signals would leave
# one stale in the stream and the awaitSettled after a nudge would mis-read it.
set -u
LOG="${FAKE_PI_PROMPT_FILE:-/tmp/fake_pi_in.jsonl}"

[ "${FAKE_PI_IGNORE_TERM:-}" = "1" ] && trap '' TERM

n=0

# RPC-style loop: read commands until stdin EOF. Each line is a command; the
# prompt (and any follow-up nudge) arrives as a "prompt" command.
while IFS= read -r line || [ -n "$line" ]; do
	echo "$line" >> "$LOG"

	case "$line" in
	*'"type":"set_auto_compaction"'*|*'"type":"set_auto_retry"'*)
		# Startup config commands: ack them (real pi responds with
		# success:true). The harness waits for these acks before the prompt.
		id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
		echo "{\"id\":\"$id\",\"type\":\"response\",\"command\":\"cfg\",\"success\":true}"
		;;
	*'"type":"prompt"'*)
		n=$((n + 1))

		if [ "${FAKE_PI_REJECT:-}" = "1" ]; then
			echo '{"type":"response","command":"prompt","success":false}'
			if [ "${FAKE_PI_NO_AGENT_END:-}" != "1" ]; then
				echo '{"type":"agent_settled"}'
			fi
			continue
		fi

		echo '{"type":"response","command":"prompt","success":true}'

		if [ "${FAKE_PI_EDIT:-}" = "1" ] && [ "$n" -eq 1 ]; then
			# Create a branch + commit so the run passes the commits-ahead gate.
			# FAKE_PI_BRANCH names the branch (the E2E test sets it to
			# bot/<runID>-security-fixes); default to a safe branch when unset.
			if [ -n "${FAKE_PI_BRANCH:-}" ]; then
				git checkout -q -b "$FAKE_PI_BRANCH"
			else
				git checkout -q -b bot/fake-fix
			fi
			printf 'edited\n' > edited.txt
			git add -A
			git -c user.name=bothos -c user.email=bothos@localhost commit -qm "apply fix (fake)"
		fi

		# Verdict hook: write .bothos/verdict.json on the configured prompt.
		if [ -n "${FAKE_PI_VERDICT:-}" ] && [ "$n" -eq "${FAKE_PI_VERDICT_ON_PROMPT:-1}" ]; then
			mkdir -p .bothos
			printf '%s\n' "$FAKE_PI_VERDICT" > .bothos/verdict.json
		fi

		# Review hook: write the content-only review result on the first prompt.
		if [ -n "${FAKE_PI_REVIEW:-}" ] && [ "$n" -eq 1 ]; then
			mkdir -p .bothos
			printf '%s\n' "$FAKE_PI_REVIEW" > .bothos/review.json
		fi

		if [ "${FAKE_PI_NO_AGENT_END:-}" != "1" ]; then
			if [ "${FAKE_PI_AGENT_END:-}" = "1" ]; then
				echo '{"type":"agent_end","willRetry":false}'
			else
				echo '{"type":"agent_settled"}'
			fi
		fi
		;;
	esac
done
exit 0
