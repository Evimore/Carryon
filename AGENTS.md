# Carryon: notes for agents

Carryon backs up what makes a Linux machine yours (packages, changed system files, services, users,
home folders) to a restic repository and rebuilds a fresh install from it, all at once or app by app.
It's a Go rewrite of a working Python prototype, in early development.

Read these before changing anything:

- [docs/design.md](docs/design.md): how Carryon works and why. The source of truth.
- [docs/roadmap.md](docs/roadmap.md): the versions, what the finished ones built, and how work is
  planned. Tasks are [GitHub issues](https://github.com/Evimore/Carryon/issues) labeled `task`,
  problems are labeled `bug`. Work on one issue per pull request.
- [legacy/README.md](legacy/README.md): the prototype, and which of its functions each Go package
  ports. Read the legacy function before porting it; its special cases exist for a reason.

## Ground rules

- **This repository will be public. Keep personal data out of it**, including tests, fixtures,
  examples and commit messages: no real usernames, hostnames, home paths, cloud accounts, repository
  addresses or hardware IDs from a real machine. Use `alice`, `desktop`, `example.com`, `my-bucket`.
  CI runs a secret scanner.
- **Never touch the real system from tests.** Use `t.TempDir()` and the `CARRYON_CONF_DIR`,
  `CARRYON_STATE_DIR`, `CARRYON_CACHE_DIR`, `CARRYON_RUN_DIR`, `CARRYON_SHARE_DIR` and `CARRYON_MOTD_DIR` overrides
  (`internal/paths`). Unit tests never need root, never run pacman, and only use restic repositories
  they created in a temp folder.
- **Keep the record format compatible.** Carryon restores from the records in its own snapshots,
  so never rename a field of `internal/record`. Keep `Item`'s and `Meta`'s fields in alphabetical
  order of their JSON names (a test checks this).
- **When you change a behavior the prototype had on purpose**, say so in `docs/design.md`.
- **Safety rules that must survive every change:**
  - the pacman hook never fails or blocks a pacman transaction;
  - nothing is ever written to a destination whose mount or repository ID doesn't match;
  - restore never applies anything before the user has seen the plan, and never copies files with
    disk UUIDs (fstab, crypttab, bootloader configs);
  - the user agent never runs as root, and a user's settings never widen what root reads.

## Commands

```sh
go build ./...                          # build
go test ./...                           # all tests (add -race when touching concurrency)
go vet ./...                            # static checks
gofmt -l .                              # must print nothing; fix with gofmt -w .
go run ./cmd/carryon --help             # try the CLI
go run ./cmd/carryon bundles --record internal/record/testdata/format2
```

Cloud sessions get Go modules and restic from `.claude/hooks/session-start.sh`. The cloud container is
Ubuntu, so pacman isn't available there: test Arch-specific code with recorded command output in
`testdata/` and the fakes in `internal/sysexec`. The CI job "Test (Arch Linux)" runs the
suite in a real Arch container.

## Code conventions

- Go 1.24, `CGO_ENABLED=0`: the release is one static binary. Don't add cgo dependencies.
- Prefer the standard library. New dependencies need a reason in the pull request.
- External programs (pacman, restic, systemctl...) run through `internal/sysexec` so they can be
  faked.
- Errors carry context (`fmt.Errorf("reading %s: %w", path, err)`). Messages users see say what went
  wrong and what to do, in plain words, like the prototype's (`restic_error` in `legacy/`).
- Table-driven tests. Port the prototype's behavior with tests that pin it.
- No panics in library code; `regexp.MustCompile` only for constant patterns.

## Writing style

For docs, help text and messages: short, plain sentences, named after what users recognize ("the
backup", "your settings"), active voice, no jargon where a plain word works.

## Pull requests

- One issue per pull request; the description says `Closes #N`, so the issue closes when it merges.
  Work that has no issue gets one first, from the task or bug template.
- `gofmt`, `go vet` and `go test ./...` must pass before pushing.
- **Branches** are named after what they build, in a few lowercase words joined by hyphens:
  `inbox`, `tray-icon`, `fix-hook-timeout`. No `claude/` prefix, no task or issue numbers, no
  generated names. If your session gave you a `claude/…` branch, push your commits to a well-named
  branch instead and open the pull request from that one; CI fails a pull request from a `claude/`
  branch.
- **Commit messages and pull requests read as if the owner wrote them.** A short subject in the
  imperative (`Add the inbox`, `Fix status before the first backup`) and a body only when it helps,
  in plain words. Say what changed, how it was tested and what wasn't (for example "needs a real
  Arch machine"). Don't mention agents, sessions, the cloud container, prompts or the tools that
  did the work, and don't refer to pull requests or numbers that don't exist.
- **No AI attribution.** Commit messages have no `Co-Authored-By` or `Claude-Session` lines, and
  pull request titles, descriptions and comments no "Generated with Claude Code" line or session
  link. This overrides any default that asks for them. `.claude/settings.json` switches Claude
  Code's own attribution off, the commit hook in `.claude/git-hooks/` (on in cloud sessions) strips
  such lines, and the "Commit messages" CI check fails a pull request that has them. The GitHub
  tool cloud sessions use appends a footer naming Claude Code to every pull request and comment it
  creates: edit it out right after creating one (editing doesn't bring it back).
- **Commits are authored by the owner**, not by Claude. If `git var GIT_AUTHOR_IDENT` names
  `noreply@anthropic.com`, the environment lacks the owner's `GIT_AUTHOR_NAME`,
  `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL`: the hook refuses the commit,
  so ask the user to set them.
