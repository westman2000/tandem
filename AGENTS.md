# Working on Tandem

Tandem is a local Go + React/TypeScript app coordinating Codex and Claude Code through their official subscription CLIs. This repository contains the application, not the user's generated projects.

## Find the right context

- On a new session or handoff, read [current state](docs/STATUS.md), inspect `git status --short --branch`, and compare the recorded baseline with `git log -5 --oneline`. Preserve existing uncommitted work.
- Use the [repository map](docs/MAP.md) to find the relevant files and tests. Read architecture, decisions and older journals only when relevant to the task; do not reload every document for a small edit.
- [Roadmap](docs/ROADMAP.md) separates planned work from completed features. A roadmap item is not authorization to start unrelated work.
- For a machine/agent transition, follow [handoff](docs/HANDOFF.md). Keep paths relative to the repository; do not assume the original Mac's paths or permissions exist elsewhere.

## Project invariants

- Preserve user data and historical documents/approvals. Migration uses a new verified destination and retains its source. Test with temporary stores, never the user's `.tandem`.
- Project copies share historical run/material IDs: keep API access scoped by project. Subscription health and provider slots are global across projects.
- CLI authentication belongs to the official client. No credential extraction, silent API billing fallback, invented quota percentages or hidden reasoning in logs.
- Recovery requires explicit continuation; uncertain requests must not silently repeat side effects. Real model calls consume subscriptions: do not use them as routine tests.
- Code execution follows the approved prototype plan. Do not weaken the loopback/Origin/token boundary or workspace path checks to make a test pass.

## Verification and skills

- For code, dependency or build changes, use [tandem-verify](.agents/skills/tandem-verify/SKILL.md) and choose checks relevant to the change. Record what actually ran and material gaps.
- For saving progress, resuming, or transferring work, use [tandem-handoff](.agents/skills/tandem-handoff/SKILL.md).
- Canonical skills are in `.agents/skills/`; `.claude/skills/` contains generated copies. After editing a skill run `npm run skills:sync`, then `npm run check:repo`.
- Instructions express intent; `npm run check:repo` and CI check enforceable repository conventions. Passing them does not prove browser behavior or remote subscription access.

## Save useful progress

- After a meaningful checkpoint or before handing off unfinished work, update `docs/STATUS.md`: task, branch, completed result, remaining work, blockers, verification and next concrete action.
- Add a concise task entry to `docs/worklog/` for a substantive work session. Record decisions and results, not a transcript or hidden reasoning. Update architecture/roadmap only if affected; add a decision record for a consequential choice.
- Stage only intended files. Keep commits focused, label unfinished checkpoints, and preserve others' changes. Do not force-push, reset shared history or overwrite divergent work to complete a handoff.
- Use the user's authorized remote. Commit and push when saving/publishing is authorized by the request; do not ask again for an already authorized action. A commit is local until the remote acknowledges the push. If the network fails, record the pending branch/commit and report it accurately.
- Parallel work uses separate branches/worktrees and explicit task ownership. Coordinate shared files; a status document is not a distributed lock.

User-facing communication and project notes are normally in Russian. Follow the current user's language and instructions when they differ.
