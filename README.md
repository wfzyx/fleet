# fleet

**A headless, copyleft daemon and control plane for autonomous coding agent fleets.**

`fleet` coordinates multi-agent coding workflows across repositories. It runs as a lightweight background daemon (`fleetd`) communicating over a local Unix domain socket (JSON-RPC 2.0), providing strict worktree isolation, spend tracking, and gatekeeping for pluggable frontends (TUI, Tauri desktop, web dashboards, or external agents).

---

## Architecture

```
   ┌────────────────────────────────────────────────────────┐
   │                   Frontends / Clients                  │
   │  • Reference TUI (fleet)   • Tauri GUI (xterm.js)     │
   │  • External Agents (Toph)  • Web Dashboards / Bots     │
   └───────────────────────────┬────────────────────────────┘
                               │ JSON-RPC 2.0 (Unix domain socket)
                               ▼
   ┌────────────────────────────────────────────────────────┐
   │                  fleet daemon (fleetd)                 │
   │  • Spaces & Project Registry  • Spend & Budget Ledger  │
   │  • GitHub & Jira Automation   • XDG Worktree Manager   │
   └───────────────────────────┬────────────────────────────┘
                               │
               The 3-Tier Agent Hierarchy (Pi Engine)
                               │
       ┌───────────────────────┼───────────────────────┐
       ▼                       ▼                       ▼
  Lead Agent             Worker Agents           Reviewer Agent
 (Fast/Haiku)          (Sonnet/Ephemeral)         (Opus/Reasoning)
 Persistent chat,      Isolated XDG worktrees,    Validates criteria,
 coordinates scope,     zero context bleed,      diff review gatekeeper,
 never touches code.    does work & exits.       authorizes PR merges.
```

### The 3-Tier Agent Model

1. **Space Lead (1 per Space / Company):**
   - **Role:** High-availability coordinator and conversational manager.
   - **Model:** Fast, cost-efficient model (e.g. `haiku-5-5`, `gemini-flash`).
   - **Invariants:** Answers questions instantly, triages issues, delegates tasks. **Never edits code or touches worktrees directly.**
2. **Workers (Multiple, Ephemeral):**
   - **Role:** Implementation contributors.
   - **Model:** Medium coding tier (e.g. `sonnet-5-5`).
   - **Invariants:** Spawned into isolated Git worktrees under `$XDG_DATA_HOME/fleet/worktrees/`. Ephemeral execution: no context bleed, no personal memory/dream inheritance. Focuses solely on the task brief and repo `AGENTS.md`, reports review, and terminates.
3. **Reviewer (Gatekeeper):**
   - **Role:** Acceptance criteria auditor and merge gatekeeper.
   - **Model:** Deep reasoning model (e.g. `claude-opus-5-5`).
   - **Invariants:** Receives acceptance criteria and unified git diff over stdin. Validates criteria point-by-point and outputs a structured `ReviewVerdict` (approved vs changes requested). Blocks pull request merging until criteria pass.

---

## Key Features

- **Decoupled Daemon Architecture:** Headless Go service listening on `$XDG_RUNTIME_DIR/fleet/fleet.sock`. Build custom GUIs (Tauri + `xterm.js`), web apps, or editor plugins on top without rewriting orchestration logic.
- **XDG Worktree Isolation:** Workers work strictly inside `$XDG_DATA_HOME/fleet/worktrees/<space>/<task>`. Your personal working tree and git index are never polluted.
- **Dollar Counter & Budgets:** Tracks token usage and real dollar spend across sessions. Enforces hard budget caps per space, per task, or fleet-wide.
- **GitHub & Jira Automation:** OAuth integration supporting draft PR creation, commit SHA consistency verification, automated squash-merging, and Jira issue synchronization.
- **Built-in Reference TUI:** High-performance Bubbletea cockpit featuring a 3-pane layout: Spaces & Tasks sidebar, full-height Lead chat, and live Diff/Inspection pane.

---

## Getting Started

### 1. Build & Install

```bash
git clone https://github.com/wfzyx/fleet.git
cd fleet
go build -o ~/.local/bin/fleet ./cmd/fleet
```

### 2. Run the Daemon

Run interactively or manage via systemd user unit:

```bash
# Run in foreground
fleet daemon

# Or install systemd user service (~/.config/systemd/user/fleet.service)
systemctl --user enable --now fleet.service
```

### 3. Register Spaces & Set Budgets

```bash
# Add a space (company/project boundary)
fleet space add reps /path/to/reps/secondaryConsolidator

# Set financial guardrails (Max budget: $50, per-task limit: $10)
fleet space budget reps --max 50.00 --task-max 10.00
```

### 4. Delegate & Steer

```bash
# Queue a task with explicit acceptance criteria
fleet delegate reps sec-con-1 "Fix JWT token refresh race" \
  --criteria "Tests pass under race detector, refresh token rotated"

# Chat with the Space Lead
fleet ping reps "What is the status of the secondary consolidator?"

# Inspect state across spaces
fleet status
fleet brief reps
fleet cost --space reps
```

### 5. Launch the Cockpit TUI

Running `fleet` with no arguments opens the interactive Bubbletea cockpit:

```bash
fleet
```
- **`[1]` Spaces:** View registered spaces and spend meters.
- **`[2]` Tasks:** Navigate the backlog and inspect active worker progress.
- **`[3]` / `[l]` Agent Window:** Full-height conversation pane with the Lead agent (`Ctrl+J` / `Shift+Enter` for newlines).
- **`[4]` Inspector:** Live unified diffs, task briefs, worker turn logs, and reviewer checklists.
- **Action Hotkeys:** `[d]` delegate, `[n]` new task, `[a]` approve/merge, `[r]` review, `[q]` quit.

---

## JSON-RPC 2.0 API

Clients communicate with the daemon via newline-delimited JSON-RPC 2.0 over `$XDG_RUNTIME_DIR/fleet/fleet.sock`:

| Method | Parameters | Description |
| :--- | :--- | :--- |
| `spaces.list` | `{}` | List all registered spaces and current spend |
| `spaces.add` | `{"id": "...", "path": "..."}` | Register a new space |
| `spaces.budget` | `{"space": "...", "max_budget_usd": 50}` | Update space budget limits |
| `tasks.list` | `{"space": "..."}` | List tasks for a space |
| `tasks.delegate` | `{"space": "...", "task": "...", "brief": "..."}` | Queue a new task with acceptance criteria |
| `tasks.spawn` | `{"task": "...", "branch": "..."}` | Launch a worker in an isolated XDG worktree |
| `tasks.diff` | `{"task": "..."}` | Retrieve unified git diff of worktree against base |
| `tasks.review` | `{"task": "..."}` | Dispatch Reviewer agent against criteria and diff |
| `tasks.merge` | `{"task": "..."}` | Squash-merge approved PR and clean up worktree |
| `leads.chat` | `{"space": "...", "text": "..."}` | Send message to Space Lead and get reply |
| `leads.history` | `{"space": "..."}` | Retrieve persistent chat transcript |
| `events.subscribe` | `{}` | Stream live agent events (`task_progress`, `review_verdict`, etc.) |

---

## License

This project is licensed under the **GNU Affero General Public License v3.0 (AGPL-3.0)**. See [LICENSE](LICENSE) for details.

*Copyleft Notice:* The AGPLv3 ensures that `fleet` remains free and open-source software. If you modify or run `fleet` as part of a network service or SaaS, you are legally required to make the complete corresponding source code available under the AGPLv3.
