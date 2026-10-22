# Contributing

Issues and pull requests are welcome. Carryon is in early development, so the most useful
contributions right now are the tasks in the [roadmap](docs/roadmap.md).

## Development

You need Go 1.24 or newer, and [restic](https://restic.net) for the tests that use a repository.

```sh
go build ./cmd/carryon
go test ./...
go vet ./...
gofmt -l .        # must print nothing
```

- Read [docs/design.md](docs/design.md) before changing behavior, and update it when a decision
  changes.
- Many features port the [prototype](legacy/README.md). Read the matching legacy function first and
  pin its behavior with tests.
- Tests never touch the real system. They use temporary folders through the `CARRYON_*_DIR`
  environment variables and never need root.
- Keep personal data out of code, tests and commits. Use placeholders such as `alice`, `desktop` and
  `example.com`.
- New behavior comes with a test. CI runs the tests on Ubuntu and in an Arch Linux container.
- Name branches after what they build (`inbox`, `fix-hook-timeout`), and write commit subjects as
  short commands (`Add the inbox`).

## End-to-end tests

The end-to-end tests back up a whole Arch Linux machine, restore it onto a fresh one and compare
the two, and run Carryon's systemd units and pacman hook for real. They run in containers booted
with systemd, so they need Docker or Podman able to run privileged containers, plus Go:

```sh
test/e2e/run.sh            # both: about 5 minutes, mostly downloading packages
test/e2e/run.sh restore    # only the backup and restore
test/e2e/run.sh systemd    # only the units and the hook
ENGINE=podman test/e2e/run.sh   # with Podman (as root: sudo -E)
KEEP=1 test/e2e/run.sh restore  # leave the containers to look at; test/e2e/run.sh clean removes them
```

They never touch your system: everything happens inside the containers and two volumes named
`carryon-e2e-*`. CI runs them on every pull request ("End-to-end"). See
[test/e2e/README.md](test/e2e/README.md) for what they check.

## Licence

By contributing you agree that your contribution is licensed under the GNU General Public License
v3.0 or later, like the rest of Carryon.
