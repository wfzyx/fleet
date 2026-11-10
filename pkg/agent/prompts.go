package agent

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// ErrInvalidPrompt is returned when prompt data is incomplete.
var ErrInvalidPrompt = errors.New("agent: invalid prompt data")

// LeadSystemPrompt is appended to Pi's system prompt for the lead.
const LeadSystemPrompt = `You are the lead of a fleet space. You triage tasks, spawn workers, request reviews and keep status current. You do not edit repository files yourself. Be brief.`

// WorkerSystemPrompt is appended to Pi's system prompt for workers.
const WorkerSystemPrompt = `You are a worker executing exactly one task in an isolated git worktree. You know only this task prompt and the repository's AGENTS.md. Stay inside the worktree, follow the repository conventions, verify your change with the project's tests and commit your work. Do not push or merge unless the task says so. If the task is blocked or ambiguous, say so plainly and stop.`

// ReviewerSystemPrompt is appended to Pi's system prompt for reviewers.
const ReviewerSystemPrompt = `You are a strict code reviewer. You judge a diff against explicit acceptance criteria and repository invariants. You cannot modify files. Your final message must contain exactly one JSON object matching the requested schema and nothing else.`

// LeadPromptData feeds the lead's initial prompt.
type LeadPromptData struct {
	Space   string
	Summary string
	Dir     string
}

// WorkerPromptData feeds the worker's task prompt.
type WorkerPromptData struct {
	Space      string
	Task       string
	Title      string
	Brief      string
	Branch     string
	BaseBranch string
	Worktree   string
	Acceptance []string
}

// ReviewPromptData feeds the reviewer's prompt.
type ReviewPromptData struct {
	Space         string
	Task          string
	Title         string
	Criteria      []string
	Diff          string
	DiffTruncated bool
}

var funcs = template.FuncMap{
	"trim": strings.TrimSpace,
	"inc":  func(i int) int { return i + 1 }, // 1-based list numbering
}

var (
	leadTmpl = template.Must(template.New("lead").Funcs(funcs).Option("missingkey=error").Parse(
		`You are the lead of space "{{.Space}}".
{{- if .Summary}}

Space summary: {{trim .Summary}}
{{- end}}
{{- if .Dir}}

Working directory: {{.Dir}}
{{- end}}

Wait for tasks from the user. Follow the fleet-lead skill.
`))

	workerTmpl = template.Must(template.New("worker").Funcs(funcs).Option("missingkey=error").Parse(
		`# Task {{.Task}}{{if .Title}}: {{.Title}}{{end}}

Space: {{.Space}}
{{- if .Branch}}
Branch: {{.Branch}}{{if .BaseBranch}} (base: {{.BaseBranch}}){{end}}
{{- end}}
{{- if .Worktree}}
Worktree: {{.Worktree}}
{{- end}}

## Brief

{{trim .Brief}}
{{- if .Acceptance}}

## Acceptance criteria
{{range $i, $c := .Acceptance}}
{{inc $i}}. {{$c}}{{end}}
{{- end}}

## Rules

- Work only inside the worktree. Follow AGENTS.md.
- Run the relevant tests before finishing and commit your work.
- When done, end with a short summary: what changed, how you verified it, anything left open.
`))

	reviewerTmpl = template.Must(template.New("reviewer").Funcs(funcs).Option("missingkey=error").Parse(
		`# Review of task {{.Task}}{{if .Title}}: {{.Title}}{{end}}

Space: {{.Space}}

## Acceptance criteria
{{range $i, $c := .Criteria}}
{{inc $i}}. {{$c}}{{end}}

## Diff
{{if .DiffTruncated}}
NOTE: the diff below was truncated. Use the read tool for the rest and mention the truncation in your summary.
{{end}}
` + "```diff" + `
{{.Diff}}
` + "```" + `

## Output

Review concurrency races, resource leaks, error handling, missing edge-case tests and every acceptance criterion.
Reply with exactly one JSON object and no other text:

` + "```json" + `
{{.Schema}}
` + "```" + `
`))
)

// RenderLeadPrompt renders the lead's initial prompt.
func RenderLeadPrompt(d LeadPromptData) (string, error) {
	if err := ValidateName("space", d.Space); err != nil {
		return "", err
	}
	return render(leadTmpl, d)
}

// RenderWorkerPrompt renders the worker's task prompt.
func RenderWorkerPrompt(d WorkerPromptData) (string, error) {
	if err := validatePair(d.Space, d.Task); err != nil {
		return "", err
	}
	if strings.TrimSpace(d.Brief) == "" {
		return "", fmt.Errorf("%w: empty brief", ErrInvalidPrompt)
	}
	return render(workerTmpl, d)
}

// RenderReviewerPrompt renders the reviewer prompt from the acceptance
// criteria and diff. maxDiffBytes > 0 truncates the diff (on a UTF-8 and line
// boundary where possible) and flags the truncation.
func RenderReviewerPrompt(d ReviewPromptData, maxDiffBytes int) (string, error) {
	if err := validatePair(d.Space, d.Task); err != nil {
		return "", err
	}
	if len(nonEmpty(d.Criteria)) == 0 {
		return "", fmt.Errorf("%w: no acceptance criteria", ErrInvalidPrompt)
	}
	if strings.TrimSpace(d.Diff) == "" {
		return "", fmt.Errorf("%w: empty diff", ErrInvalidPrompt)
	}
	d.Criteria = nonEmpty(d.Criteria)
	d.Diff, d.DiffTruncated = TruncateDiff(d.Diff, maxDiffBytes)
	return render(reviewerTmpl, struct {
		ReviewPromptData
		Schema string
	}{d, VerdictSchema})
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// TruncateDiff cuts diff to at most max bytes, preferring to end on a line
// boundary and never splitting a UTF-8 sequence. max <= 0 disables truncation.
func TruncateDiff(diff string, max int) (string, bool) {
	if max <= 0 || len(diff) <= max {
		return diff, false
	}
	cut := diff[:max]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	// Drop a rune split by the cut (only possible without a newline cut).
	return strings.ToValidUTF8(cut, ""), true
}

func render(t *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("agent: render %s prompt: %w", t.Name(), err)
	}
	return buf.String(), nil
}
