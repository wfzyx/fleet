package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"fleet/pkg/config"
	"fleet/pkg/service"
)

func (a *app) spaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "space",
		Short: "Manage spaces (list, add, remove, budget)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.spaceListCmd(), a.spaceAddCmd(), a.spaceRemoveCmd(), a.spaceBudgetCmd())
	return cmd
}

// spaceRow is one line of `fleet space list`.
type spaceRow struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Projects      []config.Project `json:"projects"`
	Lead          string           `json:"lead"`
	ActiveTasks   int              `json:"active_tasks"`
	SpendUSD      float64          `json:"spend_usd"`
	MaxBudgetUSD  float64          `json:"max_budget_usd"`
	TaskBudgetUSD float64          `json:"task_budget_usd"`
}

func money(v float64) string { return fmt.Sprintf("$%.2f", v) }

func limit(v float64) string {
	if v <= 0 {
		return "unlimited"
	}
	return money(v)
}

func (a *app) spaceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List spaces with spend, budgets and lead state",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				rows, err := spaceRows(ctx, c)
				if err != nil {
					return err
				}
				return a.emit(cmd, rows, func(w io.Writer) {
					if len(rows) == 0 {
						fmt.Fprintln(w, "no spaces; add one with `fleet space add <id> <path>`")
						return
					}
					tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
					fmt.Fprintln(tw, "SPACE\tPROJECTS\tLEAD\tACTIVE\tSPEND\tBUDGET\tPER-TASK")
					for _, r := range rows {
						var ps []string
						for _, p := range r.Projects {
							ps = append(ps, p.ID)
						}
						fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", r.ID, orDash(strings.Join(ps, ",")), r.Lead,
							r.ActiveTasks, money(r.SpendUSD), limit(r.MaxBudgetUSD), limit(r.TaskBudgetUSD))
					}
					tw.Flush()
				})
			})
		},
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func spaceRows(ctx context.Context, c *service.Client) ([]spaceRow, error) {
	spaces, err := c.Spaces(ctx)
	if err != nil {
		return nil, err
	}
	tasks, err := c.Tasks(ctx, service.TaskFilter{})
	if err != nil {
		return nil, err
	}
	leads, err := c.LeadStatuses(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]spaceRow, 0, len(spaces))
	for _, sp := range spaces {
		r := spaceRow{
			ID: sp.ID, Name: sp.Name, Projects: sp.Projects, Lead: leads[sp.ID],
			MaxBudgetUSD: sp.MaxBudgetUSD, TaskBudgetUSD: sp.TaskBudgetUSD,
		}
		if r.Lead == "" {
			r.Lead = service.LeadOffline
		}
		if r.Projects == nil {
			r.Projects = []config.Project{}
		}
		for _, t := range tasks {
			if t.SpaceID != sp.ID {
				continue
			}
			r.SpendUSD += t.SpendUSD
			if !t.Status.Terminal() {
				r.ActiveTasks++
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

func (a *app) spaceAddCmd() *cobra.Command {
	var (
		name, base, summary string
		projects            []string
		budget, taskBudget  float64
	)
	cmd := &cobra.Command{
		Use:   "add <id> [path]",
		Short: "Register a space, optionally with its first project",
		Long: `Register a space. With a path, the space gets one project named after the
space (the path must be an existing git checkout). Use --project id=path
(repeatable) for spaces with several projects.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sp := config.Space{ID: args[0], Name: name, MaxBudgetUSD: budget, TaskBudgetUSD: taskBudget, Projects: []config.Project{}}
			if len(args) == 2 {
				p, err := absPath(args[1])
				if err != nil {
					return err
				}
				sp.Projects = append(sp.Projects, config.Project{ID: args[0], Name: name, Path: p, BaseBranch: base, Summary: summary})
			}
			for _, spec := range projects {
				id, path, ok := strings.Cut(spec, "=")
				if !ok || id == "" || path == "" {
					return fmt.Errorf("--project %q: want id=path", spec)
				}
				p, err := absPath(path)
				if err != nil {
					return err
				}
				sp.Projects = append(sp.Projects, config.Project{ID: id, Path: p, BaseBranch: base, Summary: summary})
			}
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				if err := c.AddSpace(ctx, sp); err != nil {
					return err
				}
				return a.emit(cmd, sp, func(w io.Writer) {
					fmt.Fprintf(w, "added space %s (%d project(s))\n", sp.ID, len(sp.Projects))
				})
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "display name (default: the id)")
	f.StringVar(&base, "base", "", "base branch for pull requests (default: detected)")
	f.StringVar(&summary, "summary", "", "one-line description shown to the lead")
	f.StringArrayVar(&projects, "project", nil, "additional project as id=path (repeatable)")
	f.Float64Var(&budget, "budget", 0, "space spend limit in USD (0 = unlimited)")
	f.Float64Var(&taskBudget, "task-budget", 0, "per-task spend limit in USD (0 = unlimited)")
	return cmd
}

func absPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	return abs, nil
}

func (a *app) spaceRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "remove <id>",
		Aliases: []string{"rm"},
		Short:   "Remove a space and its task records",
		Long:    "Remove a space. It fails while the space has active tasks unless --force is given, which closes them first.",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				if err := c.RemoveSpace(ctx, args[0], force); err != nil {
					return err
				}
				return a.emit(cmd, map[string]string{"removed": args[0]}, func(w io.Writer) {
					fmt.Fprintf(w, "removed space %s\n", args[0])
				})
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "close active tasks and remove anyway")
	return cmd
}

func (a *app) spaceBudgetCmd() *cobra.Command {
	var maxUSD, taskMax, fleetMax float64
	cmd := &cobra.Command{
		Use:   "budget [space]",
		Short: "Show or change budgets",
		Long: `Without flags, show the space's spend against its budgets. With --max and/or
--task-max, set the space's total and per-task limits; with --fleet set the
fleet-wide limit (no space needed). A limit of 0 means unlimited.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			setFleet, setMax, setTask := fl.Changed("fleet"), fl.Changed("max"), fl.Changed("task-max")
			space := spaceArg(args)
			if !setFleet && space == "" {
				return fmt.Errorf("space is required")
			}
			if (setMax || setTask) && space == "" {
				return fmt.Errorf("--max and --task-max need a space")
			}
			return a.withClient(cmd, func(ctx context.Context, c *service.Client) error {
				if setFleet {
					if err := c.SetFleetBudget(ctx, fleetMax); err != nil {
						return err
					}
				}
				if setMax || setTask {
					var u service.BudgetUpdate
					if setMax {
						u.MaxUSD = &maxUSD
					}
					if setTask {
						u.TaskMaxUSD = &taskMax
					}
					if err := c.SetSpaceBudget(ctx, space, u); err != nil {
						return err
					}
				}
				if space == "" {
					return a.emit(cmd, map[string]float64{"fleet_max_usd": fleetMax}, func(w io.Writer) {
						fmt.Fprintf(w, "fleet budget: %s\n", limit(fleetMax))
					})
				}
				rep, err := c.Cost(ctx, space, "")
				if err != nil {
					return err
				}
				return a.emit(cmd, rep, func(w io.Writer) { printCost(w, rep) })
			})
		},
	}
	f := cmd.Flags()
	f.Float64Var(&maxUSD, "max", 0, "total space limit in USD")
	f.Float64Var(&taskMax, "task-max", 0, "per-task limit in USD")
	f.Float64Var(&fleetMax, "fleet", 0, "fleet-wide limit in USD")
	return cmd
}

func printCost(w io.Writer, r service.CostReport) {
	scope := "space " + r.Space
	if r.Task != "" {
		scope += " task " + r.Task
	}
	fmt.Fprintf(w, "%s: spent %s\n", scope, money(r.Usage.Cost.Total))
	for _, s := range r.Statuses {
		name := string(s.Scope) + " " + s.Space
		if s.Task != "" {
			name = string(s.Scope) + " " + s.Space + "/" + s.Task
		}
		fmt.Fprintf(w, "  %s: %s of %s (%s)\n", name, money(s.Spent), limit(s.Limit), s.State)
	}
	for _, m := range r.Models {
		fmt.Fprintf(w, "  %s: %d requests, %s\n", m.Key(), m.Requests, money(m.Cost.Total))
	}
	fmt.Fprintf(w, "fleet: spent %s of %s\n", money(r.Fleet.CurrentSpendUSD), limit(r.Fleet.MaxBudgetUSD))
}
