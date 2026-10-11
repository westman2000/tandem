---
name: tandem-handoff
description: Resume or hand off Tandem development across sessions, agents and machines using repository state, task notes and Git. Use when saving progress, changing agents, or continuing work after a break.
---

# Resume or hand off Tandem

Follow the applicable part of [HANDOFF](../../../docs/HANDOFF.md).

On resume, inspect the actual branch, `git status --short --branch`, recent commits and [STATUS](../../../docs/STATUS.md). Use [MAP](../../../docs/MAP.md) to read only task-relevant code and notes. Reconcile stale notes with uncommitted work before editing; a clean clone does not carry personal CLI sessions, plugins or authentication.

Before a handoff or after a meaningful checkpoint:

1. Update STATUS with task/owner, branch, checked baseline, completed behavior, remaining work, blockers, validation gaps and a concrete next action. Do not rewrite the whole history there.
2. Add or update one concise `docs/worklog/YYYY-MM-DD-task.md` entry with actual results and checks. Record consequential choices in `docs/decisions/`; do not duplicate diffs, chats or hidden reasoning. Do not try to embed a file's own future commit hash.
3. Run `npm run check:repo` and inspect the intended diff. For unfinished work, state what fails or remains untested; a labelled checkpoint in a task branch can be handed off without pretending completion.
4. Stage only intended files. Commit/push within the user's authorization. Never replace divergent remote work or force-push to make a transfer succeed. If publication was requested, continue through a normal push rather than asking again.
5. Report the branch/commit and whether push was acknowledged. If offline, retain the commit, record the pending transfer and give the retry command. Never claim GitHub has the work based only on local commit success.

For concurrent agents, record task ownership and use separate branches/worktrees. A status file is coordination information, not a lock. Handoff of Tandem source code and backup of user projects are separate operations.
