package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Verdict is the reviewer's decision.
type Verdict string

const (
	VerdictApprove        Verdict = "approve"
	VerdictRequestChanges Verdict = "request_changes"
	VerdictReject         Verdict = "reject"
)

// Severity ranks a review issue.
type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityMajor   Severity = "major"
	SeverityMinor   Severity = "minor"
	SeverityNit     Severity = "nit"
)

// CriterionResult is the reviewer's finding for one acceptance criterion.
type CriterionResult struct {
	Criterion string `json:"criterion"`
	Met       bool   `json:"met"`
	Evidence  string `json:"evidence,omitempty"`
}

// Issue is one finding in the diff.
type Issue struct {
	Severity Severity `json:"severity"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
}

// ReviewVerdict is the structured output of the reviewer.
type ReviewVerdict struct {
	Verdict  Verdict           `json:"verdict"`
	Summary  string            `json:"summary"`
	Criteria []CriterionResult `json:"criteria"`
	Issues   []Issue           `json:"issues"`
}

// VerdictSchema is the example object embedded in the reviewer prompt.
const VerdictSchema = `{
  "verdict": "approve | request_changes | reject",
  "summary": "one or two sentences",
  "criteria": [{"criterion": "<criterion text>", "met": true, "evidence": "<file:line or reasoning>"}],
  "issues": [{"severity": "blocker | major | minor | nit", "file": "path", "line": 0, "message": "<what is wrong and how to fix it>"}]
}`

// ErrNoVerdict is returned when reviewer output contains no usable JSON verdict.
var ErrNoVerdict = errors.New("agent: no review verdict found in output")

// ErrInvalidVerdict is returned when the verdict JSON is well-formed but
// semantically invalid.
var ErrInvalidVerdict = errors.New("agent: invalid review verdict")

// Validate checks the verdict's internal consistency: a known verdict, a
// summary, known severities, and that an approval has no unmet criteria and
// no blocker/major issues.
func (v *ReviewVerdict) Validate() error {
	switch v.Verdict {
	case VerdictApprove, VerdictRequestChanges, VerdictReject:
	default:
		return fmt.Errorf("%w: unknown verdict %q", ErrInvalidVerdict, v.Verdict)
	}
	if strings.TrimSpace(v.Summary) == "" {
		return fmt.Errorf("%w: empty summary", ErrInvalidVerdict)
	}
	for i, is := range v.Issues {
		switch is.Severity {
		case SeverityBlocker, SeverityMajor, SeverityMinor, SeverityNit:
		default:
			return fmt.Errorf("%w: issue %d has unknown severity %q", ErrInvalidVerdict, i, is.Severity)
		}
		if strings.TrimSpace(is.Message) == "" {
			return fmt.Errorf("%w: issue %d has no message", ErrInvalidVerdict, i)
		}
	}
	if v.Verdict == VerdictApprove {
		for _, c := range v.Criteria {
			if !c.Met {
				return fmt.Errorf("%w: approve with unmet criterion %q", ErrInvalidVerdict, c.Criterion)
			}
		}
		for _, is := range v.Issues {
			if is.Severity == SeverityBlocker || is.Severity == SeverityMajor {
				return fmt.Errorf("%w: approve with %s issue", ErrInvalidVerdict, is.Severity)
			}
		}
	}
	return nil
}

// Approved reports whether the verdict allows merging.
func (v *ReviewVerdict) Approved() bool { return v.Verdict == VerdictApprove }

// ParseReviewVerdict extracts and validates the verdict JSON from the
// reviewer's raw output. It tolerates prose and markdown fences around the
// object and uses the last top-level JSON object that has a "verdict" field.
func ParseReviewVerdict(output string) (*ReviewVerdict, error) {
	cands := jsonObjects(output)
	var lastErr error
	for i := len(cands) - 1; i >= 0; i-- {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(cands[i], &probe); err != nil {
			continue
		}
		if _, ok := probe["verdict"]; !ok {
			continue
		}
		var v ReviewVerdict
		dec := json.NewDecoder(bytes.NewReader(cands[i]))
		if err := dec.Decode(&v); err != nil {
			lastErr = fmt.Errorf("%w: %v", ErrInvalidVerdict, err)
			continue
		}
		v.Verdict = Verdict(strings.ToLower(strings.TrimSpace(string(v.Verdict))))
		if err := v.Validate(); err != nil {
			lastErr = err
			continue
		}
		return &v, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoVerdict
}

// jsonObjects returns every balanced top-level {...} span in s, honouring
// JSON string quoting so braces inside strings do not confuse the scan.
func jsonObjects(s string) [][]byte {
	var out [][]byte
	for i := 0; i < len(s); {
		if s[i] != '{' {
			i++
			continue
		}
		end := matchBrace(s, i)
		if end < 0 || !json.Valid([]byte(s[i:end+1])) {
			i++ // stray brace in prose; retry from the next character
			continue
		}
		out = append(out, []byte(s[i:end+1]))
		i = end + 1
	}
	return out
}

// matchBrace returns the index of the brace closing the one at s[start], or -1.
func matchBrace(s string, start int) int {
	depth := 0
	inStr, esc := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
