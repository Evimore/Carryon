# Carryon: notes for agents

Carryon backs up what makes a Linux machine yours (packages, changed system files, services, users,
home folders) to a restic repository and rebuilds a fresh install from it, all at once or app by app.
It's a Go rewrite of a working Python prototype, in early development.

Read these before changing anything:

- [docs/design.md](docs/design.md): how Carryon works and why. The source of truth.
- [docs/roadmap.md](docs/roadmap.md): the task list. Work on one task per pull request.
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
- **Keep the record format compatible.** `internal/record` uses the prototype's JSON field names so
  Carryon can restore from the prototype's snapshots. Never rename a field. Keep `Item`'s and
  `Meta`'s fields in alphabetical order of their JSON names (a test checks this).
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
go run ./cmd/carryon bundles --record internal/record/testdata/format1
```

Cloud sessions get Go modules and restic from `.claude/hooks/session-start.sh`. The cloud container is
Ubuntu, so pacman isn't available there: test Arch-specific code with recorded command output in
`testdata/` and the fakes in `internal/sysexec` (roadmap T01). The CI job "Test (Arch Linux)" runs the
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

- One roadmap task per pull request; mark it 🔨 or ✅ in `docs/roadmap.md`.
- `gofmt`, `go vet` and `go test ./...` must pass before pushing.
- Say what was tested and how, including what couldn't be tested in the cloud container.
