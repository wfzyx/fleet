package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"fleet/pkg/schema"
	"fleet/pkg/service"
)

// DesktopStatus is exported to $XDG_RUNTIME_DIR/fleet/status.json for widgets.
type DesktopStatus struct {
	Online        bool               `json:"online"`
	PID           int                `json:"pid"`
	SpacesCount   int                `json:"spaces_count"`
	WorkingCount  int                `json:"working_count"`
	ReviewCount   int                `json:"review_count"`
	QueuedCount   int                   `json:"queued_count"`
	TotalSpendUSD float64               `json:"total_spend_usd,omitempty"`
	Spaces        []DesktopSpaceInfo    `json:"spaces"`
	Tasks         []DesktopTaskInfo     `json:"tasks"`
	Schedules     []DesktopScheduleInfo `json:"schedules"`
	LastUpdated   time.Time             `json:"last_updated"`
}

type DesktopScheduleInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Target   string `json:"target"`
	NextRun  string `json:"next_run,omitempty"`
	LastRun  string `json:"last_run,omitempty"`
	Active   bool   `json:"active"`
}

type DesktopSpaceInfo struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Lead          string  `json:"lead"`
	ActiveTasks   int     `json:"active_tasks"`
	SpendUSD      float64 `json:"spend_usd"`
	MaxBudgetUSD  float64 `json:"max_budget_usd"`
	TaskBudgetUSD float64 `json:"task_budget_usd"`
}

type DesktopTaskInfo struct {
	ID       string        `json:"id"`
	SpaceID  string        `json:"space_id"`
	Title    string        `json:"title"`
	Status   schema.Status `json:"status"`
	Branch   string        `json:"branch,omitempty"`
	PRUrl    string        `json:"pr_url,omitempty"`
	SpendUSD float64       `json:"spend_usd"`
	Blocked  bool          `json:"blocked,omitempty"`
}

func (a *app) notifyCmd() *cobra.Command {
	var (
		sound   bool
		actions bool
		httpPort int
	)

	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Run desktop notification daemon and update widget status cache",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			c, err := a.dial(ctx)
			if err != nil {
				return fmt.Errorf("dial daemon: %w", err)
			}
			defer c.Close()

			if err := c.Subscribe(ctx); err != nil {
				return fmt.Errorf("subscribe to daemon events: %w", err)
			}

			runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
			if runtimeDir == "" {
				runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
			}
			statusPath := filepath.Join(runtimeDir, "fleet", "status.json")

			notifier := &desktopNotifier{
				client:     c,
				statusPath: statusPath,
				sound:      sound,
				actions:    actions,
			}

			// Start local HTTP server for QML widgets and actions
			if httpPort > 0 {
				notifier.startHTTPServer(ctx, fmt.Sprintf("127.0.0.1:%d", httpPort))
			}

			// Initial status refresh
			notifier.refreshStatus(ctx)

			// Start background status poll ticker (every 3 seconds)
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()

			slog.Info("fleet notify: listening for events and maintaining status cache", "status_path", statusPath, "http_port", httpPort)

			for {
				select {
				case <-ctx.Done():
					// Mark offline on exit
					notifier.writeOffline()
					return nil
				case <-ticker.C:
					notifier.refreshStatus(ctx)
				case ev, ok := <-c.Events():
					if !ok {
						notifier.writeOffline()
						return nil
					}
					notifier.handleEvent(ctx, ev)
					notifier.refreshStatus(ctx)
				}
			}
		},
	}

	cmd.Flags().BoolVar(&sound, "sound", true, "play sound on important notifications")
	cmd.Flags().BoolVar(&actions, "actions", true, "enable interactive action buttons on notifications")
	cmd.Flags().IntVar(&httpPort, "http", 2978, "local HTTP port for KDE widget bridge")
	return cmd
}

type desktopNotifier struct {
	client     *service.Client
	statusPath string
	sound      bool
	actions    bool
	cachedJSON []byte
	mu         sync.Mutex
}

func (n *desktopNotifier) startHTTPServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		n.mu.Lock()
		data := n.cachedJSON
		n.mu.Unlock()
		if len(data) == 0 {
			w.Write([]byte(`{"online":true,"tasks":[],"spaces":[]}`))
			return
		}
		w.Write(data)
	})

	mux.HandleFunc("/action", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		act := r.URL.Query().Get("type")
		task := r.URL.Query().Get("task")
		ev := schema.AgentEvent{TaskID: task}
		go n.executeAction(act, ev)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	go func() {
		_ = server.ListenAndServe()
	}()
}

func (n *desktopNotifier) refreshStatus(ctx context.Context) {
	st, err := n.client.Status(ctx)
	if err != nil {
		return
	}

	tasks, err := n.client.Tasks(ctx, service.TaskFilter{})
	if err != nil {
		return
	}

	spaces, err := n.client.Spaces(ctx)
	if err != nil {
		return
	}

	leadStatuses, _ := n.client.LeadStatuses(ctx)

	var workingCount, reviewCount, queuedCount int
	var totalSpend float64

	dTasks := make([]DesktopTaskInfo, 0, len(tasks))
	for _, t := range tasks {
		switch t.Status {
		case schema.StatusWorking:
			workingCount++
		case schema.StatusReview:
			reviewCount++
		case schema.StatusQueued:
			queuedCount++
		}
		totalSpend += t.SpendUSD
		dTasks = append(dTasks, DesktopTaskInfo{
			ID:       t.ID,
			SpaceID:  t.SpaceID,
			Title:    t.Title,
			Status:   t.Status,
			Branch:   t.Branch,
			PRUrl:    t.PRUrl,
			SpendUSD: t.SpendUSD,
		})
	}

	dSpaces := make([]DesktopSpaceInfo, 0, len(spaces))
	for _, s := range spaces {
		lead := "offline"
		if ls, ok := leadStatuses[s.ID]; ok {
			lead = ls
		}
		var spaceSpend float64
		if rep, err := n.client.Cost(ctx, s.ID, ""); err == nil {
			spaceSpend = rep.Usage.Cost.Total
		}
		dSpaces = append(dSpaces, DesktopSpaceInfo{
			ID:            s.ID,
			Name:          s.Name,
			Lead:          lead,
			SpendUSD:      spaceSpend,
			MaxBudgetUSD:  s.MaxBudgetUSD,
			TaskBudgetUSD: s.TaskBudgetUSD,
		})
	}

	ds := DesktopStatus{
		Online:        true,
		PID:           st.PID,
		SpacesCount:   len(spaces),
		WorkingCount:  workingCount,
		ReviewCount:   reviewCount,
		QueuedCount:   queuedCount,
		TotalSpendUSD: totalSpend,
		Spaces:        dSpaces,
		Tasks:         dTasks,
		Schedules:     collectSchedules(),
		LastUpdated:   time.Now().UTC(),
	}

	n.writeStatus(ds)
}

func (n *desktopNotifier) writeOffline() {
	ds := DesktopStatus{
		Online:      false,
		LastUpdated: time.Now().UTC(),
	}
	n.writeStatus(ds)
}

func (n *desktopNotifier) writeStatus(ds DesktopStatus) {
	n.mu.Lock()
	defer n.mu.Unlock()

	data, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		return
	}

	n.cachedJSON = data

	_ = os.MkdirAll(filepath.Dir(n.statusPath), 0700)
	tmp := n.statusPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err == nil {
		_ = os.Rename(tmp, n.statusPath)
	}
}

func collectSchedules() []DesktopScheduleInfo {
	var scheds []DesktopScheduleInfo

	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, _ := os.UserHomeDir()
		configDir = filepath.Join(home, ".config")
	}
	customPath := filepath.Join(configDir, "fleet", "schedules.json")
	if data, err := os.ReadFile(customPath); err == nil {
		var list []DesktopScheduleInfo
		if err := json.Unmarshal(data, &list); err == nil {
			scheds = append(scheds, list...)
		}
	}

	cmd := exec.Command("systemctl", "--user", "list-timers", "--output=json")
	out, err := cmd.Output()
	if err == nil {
		var rawTimers []struct {
			Unit      string `json:"unit"`
			Activates string `json:"activates"`
			Next      *int64 `json:"next"`
			Left      *int64 `json:"left"`
			Last      *int64 `json:"last"`
		}
		if err := json.Unmarshal(out, &rawTimers); err == nil {
			for _, rt := range rawTimers {
				if strings.HasPrefix(rt.Unit, "fleet-") {
					name := strings.TrimSuffix(rt.Unit, ".timer")
					name = strings.ReplaceAll(name, "-", " ")

					var nextStr, lastStr string
					if rt.Next != nil && *rt.Next > 0 {
						t := time.UnixMicro(*rt.Next).Local()
						nextStr = t.Format("Mon 15:04")
					} else {
						nextStr = "inactive"
					}
					if rt.Last != nil && *rt.Last > 0 {
						t := time.UnixMicro(*rt.Last).Local()
						lastStr = t.Format("Mon 15:04")
					}

					scheds = append(scheds, DesktopScheduleInfo{
						ID:       rt.Unit,
						Name:     name,
						Schedule: "Timer (" + rt.Unit + ")",
						Target:   rt.Activates,
						NextRun:  nextStr,
						LastRun:  lastStr,
						Active:   rt.Next != nil && *rt.Next > 0,
					})
				}
			}
		}
	}

	return scheds
}

func (n *desktopNotifier) handleEvent(ctx context.Context, ev schema.AgentEvent) {
	var (
		summary string
		body    string
		icon    = "applications-development"
		urgency = "normal"
		acts    []string
	)

	switch ev.Type {
	case schema.EventTaskStarted:
		summary = fmt.Sprintf("Worker Started: %s", ev.TaskID)
		body = fmt.Sprintf("Space: %s\nRole: %s", ev.SpaceID, ev.Role)
		if ev.Message != "" {
			body += "\n" + ev.Message
		}
		icon = "system-run"

	case schema.EventTaskReviewRequested:
		summary = fmt.Sprintf("Review Requested: %s", ev.TaskID)
		body = fmt.Sprintf("Worker completed implementation in space %s.\nReady for quality gate review.", ev.SpaceID)
		icon = "document-edit"
		if n.actions {
			acts = append(acts, "review=Run Reviewer")
		}

	case schema.EventReviewVerdict:
		if ev.Verdict != nil && ev.Verdict.Verdict == schema.DecisionApproved {
			summary = fmt.Sprintf("Review Approved: %s", ev.TaskID)
			body = "All acceptance criteria verified. Zero blockers.\nReady to squash-merge."
			icon = "emblem-checked"
			if n.actions {
				acts = append(acts, "merge=Merge Task")
			}
		} else {
			summary = fmt.Sprintf("Changes Requested: %s", ev.TaskID)
			notes := "Reviewer found blockers."
			if ev.Verdict != nil && ev.Verdict.Notes != "" {
				notes = ev.Verdict.Notes
			}
			body = fmt.Sprintf("Space: %s\n%s", ev.SpaceID, notes)
			urgency = "critical"
			icon = "dialog-warning"
		}

	case schema.EventTaskCompleted:
		summary = fmt.Sprintf("Task Completed: %s", ev.TaskID)
		body = fmt.Sprintf("Task closed and merged in space %s.", ev.SpaceID)
		icon = "task-complete"

	default:
		return
	}

	// Dispatch notification asynchronously so waiting on action does not block event loop
	go n.sendNotification(summary, body, icon, urgency, acts, ev)
}

func (n *desktopNotifier) sendNotification(summary, body, icon, urgency string, actions []string, ev schema.AgentEvent) {
	args := []string{
		"-a", "Fleet",
		"-i", icon,
		"-u", urgency,
	}

	if n.sound && urgency == "critical" {
		args = append(args, "-h", "string:sound-name:dialog-warning")
	} else if n.sound {
		args = append(args, "-h", "string:sound-name:message-new-instant")
	}

	for _, act := range actions {
		args = append(args, "-A", act)
	}

	args = append(args, summary, body)

	cmd := exec.Command("notify-send", args...)
	out, err := cmd.Output()
	if err != nil {
		return
	}

	action := strings.TrimSpace(string(out))
	if action == "" {
		return
	}

	n.executeAction(action, ev)
}

func (n *desktopNotifier) executeAction(action string, ev schema.AgentEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch action {
	case "terminal":
		_ = exec.Command("kitty", "-e", "fleet").Start()

	case "review":
		n.sendNotification("Review Started", fmt.Sprintf("Quality gate reviewer executing for %s...", ev.TaskID), "document-edit", "normal", nil, ev)
		res, err := n.client.Review(ctx, ev.TaskID)
		if err != nil {
			n.sendNotification("Review Failed", fmt.Sprintf("Review error on %s: %v", ev.TaskID, err), "dialog-error", "critical", nil, ev)
			return
		}
		if res.Verdict.Verdict == schema.DecisionApproved {
			n.sendNotification("Review Passed", fmt.Sprintf("%s approved! Ready to merge.", ev.TaskID), "emblem-checked", "normal", []string{"merge=Merge Now"}, ev)
		} else {
			n.sendNotification("Review Rejected", fmt.Sprintf("%s has issues: %s", ev.TaskID, res.Verdict.Notes), "dialog-warning", "critical", nil, ev)
		}

	case "merge":
		n.sendNotification("Merging Task", fmt.Sprintf("Executing squash-merge for %s...", ev.TaskID), "task-complete", "normal", nil, ev)
		_, err := n.client.Merge(ctx, ev.TaskID)
		if err != nil {
			n.sendNotification("Merge Failed", fmt.Sprintf("Failed to merge %s: %v", ev.TaskID, err), "dialog-error", "critical", nil, ev)
			return
		}
		n.sendNotification("Merge Complete", fmt.Sprintf("Task %s successfully merged!", ev.TaskID), "emblem-checked", "normal", nil, ev)
	}
}
