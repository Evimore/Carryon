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

## Licence

By contributing you agree that your contribution is licensed under the GNU General Public License
v3.0 or later, like the rest of Carryon.
