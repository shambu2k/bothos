package verifier

import (
	"fmt"
	"strings"
)

// FormatFeedback renders the verifier's findings for one agent prompt. It is
// bounded to 8000 chars, marks each failure as repeat (present in prev) or new,
// and instructs the agent how to proceed.
func FormatFeedback(prev, cur []Failure, round, maxRounds int) string {
	prevSig := make(map[string]bool, len(prev))
	for _, f := range prev {
		prevSig[f.Rule+"|"+f.Detail] = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "An external verifier re-checked your work and found the following (round %d/%d):\n", round, maxRounds)
	if len(cur) == 0 {
		b.WriteString("- (no failures)\n")
	}
	for _, f := range cur {
		mark := "new"
		if prevSig[f.Rule+"|"+f.Detail] {
			mark = "repeat"
		}
		b.WriteString(fmt.Sprintf("- [%s] %s: %s", mark, f.Rule, f.Detail))
		if f.Snippet != "" {
			b.WriteString("\n  " + strings.ReplaceAll(f.Snippet, "\n", "\n  "))
		}
		b.WriteString("\n")
	}
	b.WriteString("Please fix these, commit on the SAME branch, re-run the scanner to confirm the findings are gone, and update .bothos/verdict.json.")
	return truncStr(b.String(), 8000)
}
