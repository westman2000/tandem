---
name: tandem-verify
description: Verify Tandem code, storage, UI or build changes with appropriate local checks and record their limits. Use when preparing a Tandem implementation for review or handoff.
---

# Verify Tandem changes

Work from the repository root. Use [MAP](../../../docs/MAP.md) to locate affected tests. Do not exercise the user's actual databases or send real subscription requests as routine validation.

- Go/backend changes: run relevant scenario tests while editing; before handoff run `go test -race ./...` and `go vet ./...`. If the environment cannot write the default cache, use `GOCACHE="$PWD/.tools/go-build"`. A denied port bind or unavailable CLI is an environment limitation, not a successful integration check.
- UI/client changes: `npm test` and `npm run build` cover Node/React SSR and TypeScript/Vite. For interaction changes, use a browser with disposable data when available. Record browser gaps explicitly; SSR does not validate the Safari file picker.
- Docs/instruction changes: `npm run check:repo` and `git diff --check`; code tests are unnecessary unless executable behavior changed. After changing a canonical skill, first run `npm run skills:sync`.
- Build/platform changes: use `scripts/build-release.sh` when cross-platform verification is relevant. It compiles five targets; report separately whether they were executed. A fresh native binary requires `go build -o tandem .` after the UI build (`tandem.exe` on Windows).

`npm run build` removes tracked `web/dist/placeholder.txt`; restore only that file from HEAD if it had no intentional edits. Build outputs, caches and dependencies stay untracked. A fresh environment uses `go mod download` and `npm ci`; do not use dependency-upgrading commands just to reproduce it.

Record commands, outcomes and material untested behavior in the task's worklog; update [validation](../../../docs/validation.md) when product verification changes. Do not turn a passed compilation or mocked provider response into a claim of real platform/account compatibility. Once relevant checks pass, stop repeating them without a new reason.
