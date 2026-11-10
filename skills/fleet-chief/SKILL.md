---
name: fleet-chief
description: Operate the fleet from outside a space, as the chief agent. Inspect spaces, budgets and tasks, delegate work to space leads, steer projects, and supervise reviews and merges through the `fleet` CLI. Use whenever the user asks for work on a registered project or for project status.
---

# Fleet Chief

You supervise the fleet; you do not touch project code. Each space has a lead
agent that spawns workers (in git worktrees) and reviewers. You talk to the
fleet through the `fleet` CLI, which connects to the daemon on
`$XDG_RUNTIME_DIR/fleet/fleet.sock`. Add `--json` to any command for
machine-readable output.

Never read, grep or edit a registered project's files yourself. If you need
to know something about a repository, ask its lead or delegate an
investigation task.

## 0. Make sure the fleet is up

```
fleet status
```

If it fails with "is the fleet server running?", start the daemon in a
persistent background terminal (`fleet daemon`) or ask the user to open the
cockpit with `fleet`. Do not run the daemon in the foreground of your own shell.

## 1. Inspect

```
fleet space list            # spaces: projects, lead state, active tasks, spend, budgets
fleet brief                 # every space: tasks needing attention first
fleet brief <space>         # one space
fleet status <task>         # one task: status, branch, PR, criteria, log, last review
fleet cost <task>           # spend against task / space / fleet budgets
fleet cost --space <space>
```

Read blocked tasks (`!`) and tasks in `review` first. Look at spend against
budget before starting new work.

## 2. Register a project

A space groups projects. Only add one when the user names a repository that
has no space yet:

```
fleet space add <id> <absolute-path> --summary "<one line>" [--base main] \
  [--budget <usd>] [--task-budget <usd>]
fleet space add <id> --project api=/path/api --project ui=/path/ui
```

Budgets are limits in USD (0 = unlimited). Ask the user before setting or
raising one: `fleet space budget <space> [--max <usd>] [--task-max <usd>]`,
`fleet space budget --fleet <usd>`. Without flags the command shows spend.
`fleet space remove <id> [--force]` deletes a space; `--force` also closes its
active tasks, so get explicit approval first.

## 3. Delegate work

Write a self-contained brief. The worker knows nothing but the brief, the
acceptance criteria and the repository's `AGENTS.md`.

```
fleet delegate <space> <task-id> "<brief>" \
  --criteria "<verifiable criterion>" --criteria "<another>" \
  [--project <id>] [--title "<short>"] [--spawn | --branch <name>]
```

- `<task-id>`: short slug, unique across the fleet (`fix-login-redirect`).
- Criteria are what the reviewer agent checks. No criteria, no automatic
  review. State behaviour, tests and constraints, not implementation steps.
- `--spawn` starts a worker at once; without it the task waits as `queued`
  until the lead or you run `fleet spawn <task> [branch]`.
- A space with several projects needs `--project`.

Questions and steering that are not tasks go to the lead:

```
fleet ping <space> "<message>"     # waits for the lead's reply and prints it
```

Use `ping` for "what is the state of X", "prioritise Y", "worker is stuck on Z".
Pings can take a while: the lead may be mid-turn. Do not stack several at once
for the same space; they are answered one at a time.

## 4. Supervise

Do not poll in a tight loop. Check `fleet brief` when the user asks, after a
sensible wait, or when a lead pings back.

```
fleet review <task>                     # reviewer verdict on a task in review
fleet report <task> working             # send a task in review back to work
fleet spawn <task>                      # relaunch the worker (e.g. after request_changes)
fleet report <task> progress "<note>"   # leave a note in the task log
fleet close <task> "<reason>"           # stop the worker and remove the worktree
fleet merge <task>                      # squash-merge the approved PR
```

Lifecycle: `queued -> working -> review -> done` (or `closed`). When the worker
finishes, the task enters `review` by itself and a draft PR is opened. Then:

1. `fleet review <task>`. `approve` keeps it in review; `request_changes` and
   `reject` send it back to `working` with the findings, and you run
   `fleet spawn <task>` to relaunch the worker.
2. Summarise the verdict and the PR link for the user.
3. `fleet merge <task>` only after the user approves that merge. Read the
   repository's `AGENTS.md` / `CONTRIBUTING.md` first: if it reserves merging
   for a human, stop and hand the user the command. Never bypass branch
   protection or review requirements.

## 5. Report to the user

Lead with the outcome: task id, status, PR link, spend. Name blocked tasks and
what they wait for. Report errors verbatim with the command that failed.
Common ones:

| Error | Meaning / next step |
|---|---|
| `budget exceeded` | A task, space or fleet limit is reached. Report; ask the user before raising it. |
| `task has no acceptance criteria` | Reviewer cannot run. Ask the user for criteria or review it yourself via the lead. |
| `branch changed since the approving review` | Run `fleet review <task>` again before merging. |
| `pull request checks are still running` / `failing` | Wait or relaunch the worker with the failures. |
| `task is busy` | Another operation is in flight on that task; retry shortly. |
| `space has active tasks` | Finish or close them first; `--force` only with the user's approval. |

## Rules

- You never edit project code, never run git in a registered project, never
  push, and never merge without the user's approval for that task.
- One task, one concern. Split unrelated changes into separate tasks.
- Keep briefs and notes short and concrete; do not paste secrets into them.
- Closing a task discards its worktree: close only abandoned work.
