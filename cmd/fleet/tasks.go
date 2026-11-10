package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"fleet/pkg/schema"
	"fleet/pkg/service"
)

// spaceArg returns the first argument, or $FLEET_SPACE (set for leads).
func spaceArg(args []string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	return os.Getenv("FLEET_SPACE")
}

func (a *app) delegateCmd() *cobra.Command {
	var (
		criteria            []string
		branch, title, proj string
		jira                string
		spawn               bool
	)
	cmd := &cobra.Command{
		Use:   `delegate <space> <task> "<brief>"`,
		Short: "Queue a task in a space",
		Long: `Create a queued task in a space. The brief is the worker's only instruction:
put every constraint in it. Use --criteria (repeatable) for acceptance
criteria; the reviewer judges the diff against them.

The task stays queued until a worker is started. Use --spawn to start one
right away, or --branch <name> to start one on that branch (implies --spawn).`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				t, err := c.Delegate(ctx, service.DelegateParams{
					Space: args[0], Task: args[1], Brief: args[2], Criteria: criteria,
					Title: title, Project: proj, JiraKey: jira,
				})
				if err != nil {
					return err
				}
				if spawn || branch != "" {
					started, err := c.Spawn(ctx, t.ID, branch)
					if err != nil {
						return fmt.Errorf("task %s was created but the worker did not start: %w", t.ID, err)
					}
					t = started
				}
				return a.emit(cmd, t, func(w io.Writer) { printTaskLine(w, "delegated", t) })
			})
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&criteria, "criteria", nil, "acceptance criterion (repeatable)")
	f.StringVar(&branch, "branch", "", "start the worker on this branch (implies --spawn)")
	f.BoolVar(&spawn, "spawn", false, "start a worker immediately")
	f.StringVar(&title, "title", "", "short title (default: derived from the brief)")
	f.StringVar(&proj, "project", "", "project id (required when the space has several)")
	f.StringVar(&jira, "jira", "", "Jira issue key to keep in sync")
	return cmd
}

func printTaskLine(w io.Writer, verb string, t schema.Task) {
	fmt.Fprintf(w, "%s %s [%s] %s", verb, t.ID, t.Status, t.Title)
	if t.Branch != "" {
		fmt.Fprintf(w, " (branch %s)", t.Branch)
	}
	if t.PRUrl != "" {
		fmt.Fprintf(w, " %s", t.PRUrl)
	}
	fmt.Fprintln(w)
}

func (a *app) spawnCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "spawn <task> [branch]",
		Short: "Start (or relaunch) the worker of a task in its own worktree",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			branch := ""
			if len(args) == 2 {
				branch = args[1]
			}
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				t, err := c.Spawn(ctx, args[0], branch)
				if err != nil {
					return err
				}
				return a.emit(cmd, t, func(w io.Writer) { printTaskLine(w, "spawned", t) })
			})
		},
	}
}

func (a *app) reportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `report <task> <status> ["<note>"]`,
		Short: "Report task progress or move it to a new status",
		Long: `Report on a task. Status is one of:
  progress   log a note (note required)
  blocked    flag the task as blocked (note required)
  working    send a task in review back to work
  queued     release a task and stop its worker
  review     request review (pushes the branch and opens a draft PR)
  done       finish a reviewed task that has no pull request
  closed     abandon the task`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			note := ""
			if len(args) == 3 {
				note = args[2]
			}
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				t, err := c.Report(ctx, args[0], args[1], note)
				if err != nil {
					return err
				}
				return a.emit(cmd, t, func(w io.Writer) { printTaskLine(w, "reported", t) })
			})
		},
	}
}

func (a *app) reviewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "review <task>",
		Short: "Run the reviewer on a task that is in review",
		Long: `Run the reviewer on the task's diff against its acceptance criteria and print
the verdict. request_changes and reject send the task back to work with the
findings attached; approve leaves it in review, ready for merge.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				r, err := c.Review(ctx, args[0])
				if err != nil {
					return err
				}
				return a.emit(cmd, r, func(w io.Writer) {
					fmt.Fprintf(w, "verdict: %s (%s)\n%s\n", r.Detail.Verdict, r.Verdict.Verdict, r.Detail.Summary)
					for _, cr := range r.Detail.Criteria {
						mark := "met  "
						if !cr.Met {
							mark = "UNMET"
						}
						fmt.Fprintf(w, "  [%s] %s\n", mark, cr.Criterion)
					}
					for _, is := range r.Detail.Issues {
						loc := is.File
						if loc != "" && is.Line > 0 {
							loc = fmt.Sprintf("%s:%d", loc, is.Line)
						}
						fmt.Fprintf(w, "  - %s %s %s\n", is.Severity, loc, is.Message)
					}
					fmt.Fprintf(w, "review cost: %s; reviewed %.12s\n", money(r.CostUSD), r.SHA)
				})
			})
		},
	}
}

func (a *app) mergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <task>",
		Short: "Squash-merge the approved pull request of a task",
		Long: `Merge the task's pull request. The latest review must have approved the
branch head, checks must pass and the PR must not conflict. Done tasks have
their worktree removed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				t, err := c.Merge(ctx, args[0])
				if err != nil {
					return err
				}
				return a.emit(cmd, t, func(w io.Writer) { printTaskLine(w, "merged", t) })
			})
		},
	}
}

func (a *app) closeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `close <task> ["<note>"]`,
		Short: "Abandon a task: stop its worker and remove its worktree",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			note := ""
			if len(args) == 2 {
				note = args[1]
			}
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				t, err := c.CloseTask(ctx, args[0], note)
				if err != nil {
					return err
				}
				return a.emit(cmd, t, func(w io.Writer) { printTaskLine(w, "closed", t) })
			})
		},
	}
}

func (a *app) pingCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `ping <space> "<message>"`,
		Short: "Send a message to a space's lead and print its reply",
		Long: `Send a message to the space's lead agent (starting it if needed) and wait for
its answer. Use it for questions and steering that are not tasks.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				reply, err := c.Chat(ctx, args[0], args[1])
				if err != nil {
					return err
				}
				return a.emit(cmd, service.ChatResult{Reply: reply}, func(w io.Writer) { fmt.Fprintln(w, reply) })
			})
		},
	}
}

func (a *app) costCmd() *cobra.Command {
	var space string
	cmd := &cobra.Command{
		Use:   "cost [task]",
		Short: "Show spend against budgets for a task, or for a whole space with --space",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				task := ""
				if len(args) == 1 {
					task = args[0]
					rec, err := c.GetTask(ctx, task)
					if err != nil {
						return err
					}
					space = rec.Task.SpaceID
				} else if space == "" {
					space = os.Getenv("FLEET_SPACE")
				}
				if space == "" {
					return errors.New("give a task, --space, or set FLEET_SPACE")
				}
				rep, err := c.Cost(ctx, space, task)
				if err != nil {
					return err
				}
				return a.emit(cmd, rep, func(w io.Writer) { printCost(w, rep) })
			})
		},
	}
	cmd.Flags().StringVar(&space, "space", "", "space to report on (default $FLEET_SPACE)")
	return cmd
}

// ---------------------------------------------------------------- status & brief

// taskStatus is the JSON form of `fleet status <task>`.
type taskStatus struct {
	Task    schema.Task            `json:"task"`
	Blocked bool                   `json:"blocked"`
	Base    string                 `json:"base_branch,omitempty"`
	Log     []service.LogEntry     `json:"log"`
	Reviews []service.ReviewRecord `json:"reviews"`
}

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [task]",
		Short: "Show daemon status, or the full state of one task",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				if len(args) == 1 {
					rec, err := c.GetTask(ctx, args[0])
					if err != nil {
						return err
					}
					st := taskStatus{Task: rec.Task, Blocked: rec.Blocked, Base: rec.BaseBranch, Log: rec.Log, Reviews: rec.Reviews}
					return a.emit(cmd, st, func(w io.Writer) { printTaskStatus(w, rec) })
				}
				st, err := c.Status(ctx)
				if err != nil {
					return err
				}
				rows, err := spaceRows(ctx, c)
				if err != nil {
					return err
				}
				out := struct {
					service.Status
					Spaces []spaceRow `json:"space_list"`
				}{st, rows}
				return a.emit(cmd, out, func(w io.Writer) {
					fmt.Fprintf(w, "daemon pid %d: %d space(s), %d task(s), %d active\n", st.PID, st.Spaces, st.Tasks, st.Active)
					for _, r := range rows {
						fmt.Fprintf(w, "  %s: lead %s, %d active, %s of %s\n", r.ID, r.Lead, r.ActiveTasks, money(r.SpendUSD), limit(r.MaxBudgetUSD))
					}
				})
			})
		},
	}
}

func printTaskStatus(w io.Writer, rec service.TaskRecord) {
	t := rec.Task
	fmt.Fprintf(w, "%s: %s\n", t.ID, t.Title)
	fmt.Fprintf(w, "  space:   %s", t.SpaceID)
	if t.ProjectID != "" {
		fmt.Fprintf(w, " / %s", t.ProjectID)
	}
	fmt.Fprintln(w)
	status := string(t.Status)
	if rec.Blocked {
		status += " (blocked)"
	}
	fmt.Fprintf(w, "  status:  %s\n  spend:   %s\n", status, money(t.SpendUSD))
	if t.Branch != "" {
		fmt.Fprintf(w, "  branch:  %s (base %s)\n", t.Branch, rec.BaseBranch)
	}
	if t.WorktreePath != "" {
		fmt.Fprintf(w, "  worktree: %s\n", t.WorktreePath)
	}
	if t.PRUrl != "" {
		fmt.Fprintf(w, "  pr:      %s\n", t.PRUrl)
	}
	if t.JiraKey != "" {
		fmt.Fprintf(w, "  jira:    %s\n", t.JiraKey)
	}
	if len(t.AcceptanceCriteria) > 0 {
		fmt.Fprintln(w, "  criteria:")
		for _, c := range t.AcceptanceCriteria {
			fmt.Fprintf(w, "    - %s\n", c)
		}
	}
	if n := len(rec.Reviews); n > 0 {
		r := rec.Reviews[n-1]
		fmt.Fprintf(w, "  last review: %s: %s\n", r.Verdict, r.Summary)
	}
	logs := rec.Log
	if len(logs) > 10 {
		logs = logs[len(logs)-10:]
	}
	if len(logs) > 0 {
		fmt.Fprintln(w, "  log:")
		for _, e := range logs {
			fmt.Fprintf(w, "    %s %-8s %s\n", e.Time.Local().Format("15:04:05"), e.Role, e.Note)
		}
	}
}

// briefTask is one task in `fleet brief`.
type briefTask struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Status   schema.Status `json:"status"`
	Blocked  bool          `json:"blocked,omitempty"`
	Branch   string        `json:"branch,omitempty"`
	PRUrl    string        `json:"pr_url,omitempty"`
	SpendUSD float64       `json:"spend_usd"`
	LastNote string        `json:"last_note,omitempty"`
}

type briefSpace struct {
	spaceRow
	Tasks []briefTask `json:"tasks"`
}

func (a *app) briefCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "brief [space]",
		Short: "Summarise spaces: lead state, spend, and active tasks needing attention",
		Long: `Summarise one space (default $FLEET_SPACE) or, when none is given, every
space: lead state, spend against budget, and each active task with its last
note. Blocked tasks and tasks waiting in review are listed first.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				rows, err := spaceRows(ctx, c)
				if err != nil {
					return err
				}
				if want := spaceArg(args); want != "" {
					var kept []spaceRow
					for _, r := range rows {
						if r.ID == want {
							kept = append(kept, r)
						}
					}
					if len(kept) == 0 {
						return fmt.Errorf("%w: %q", service.ErrNoSuchSpace, want)
					}
					rows = kept
				}
				out := make([]briefSpace, 0, len(rows))
				for _, r := range rows {
					bs := briefSpace{spaceRow: r, Tasks: []briefTask{}}
					tasks, err := c.Tasks(ctx, service.TaskFilter{Space: r.ID})
					if err != nil {
						return err
					}
					for _, t := range tasks {
						if t.Status.Terminal() {
							continue
						}
						bt := briefTask{ID: t.ID, Title: t.Title, Status: t.Status, Branch: t.Branch, PRUrl: t.PRUrl, SpendUSD: t.SpendUSD}
						if rec, err := c.GetTask(ctx, t.ID); err == nil {
							bt.Blocked = rec.Blocked
							if n := len(rec.Log); n > 0 {
								bt.LastNote = rec.Log[n-1].Note
							}
						}
						bs.Tasks = append(bs.Tasks, bt)
					}
					sort.SliceStable(bs.Tasks, func(i, j int) bool { return attention(bs.Tasks[i]) < attention(bs.Tasks[j]) })
					out = append(out, bs)
				}
				return a.emit(cmd, out, func(w io.Writer) { printBrief(w, out) })
			})
		},
	}
}

// attention orders tasks: blocked first, then review, working, queued.
func attention(t briefTask) int {
	switch {
	case t.Blocked:
		return 0
	case t.Status == schema.StatusReview:
		return 1
	case t.Status == schema.StatusWorking:
		return 2
	}
	return 3
}

func printBrief(w io.Writer, spaces []briefSpace) {
	if len(spaces) == 0 {
		fmt.Fprintln(w, "no spaces")
		return
	}
	for i, s := range spaces {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s: lead %s, spend %s of %s, %d active task(s)\n", s.ID, s.Lead, money(s.SpendUSD), limit(s.MaxBudgetUSD), len(s.Tasks))
		if len(s.Tasks) == 0 {
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, t := range s.Tasks {
			st := string(t.Status)
			if t.Blocked {
				st += "!"
			}
			extra := strings.TrimSpace(t.PRUrl + " " + t.LastNote)
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", t.ID, st, money(t.SpendUSD), t.Title, extra)
		}
		tw.Flush()
	}
}
