# The app catalog

What Carryon knows about apps: which of their files to leave out of the backup, how those come back
after a restore, and what to tell the user once their settings are restored. Each app is one TOML
file here. The package installs them to `/usr/share/carryon/catalog/`, and the code that reads them
is in [`internal/catalog`](../internal/catalog).

This folder also holds `excludes.default`, the default exclude list, installed as
`/usr/share/carryon/excludes.default`. It only has what every machine has (caches, the trash); what
belongs to one app goes in that app's file.

Contributions welcome: one app per pull request is easiest to review.

## An example

```toml
# rust.toml: comments are welcome, say why something is left out.
id = "rust"                       # the file name without ".toml"
name = "Rust (rustup and cargo)"  # what users call it

[recognize]
packages = ["rustup", "rust", "cargo"]
paths = [".rustup", ".cargo"]

[[redownload]]
label = "Rust toolchains"
how = "`rustup toolchain install <name>` (cargo does it on first use)"
details = "entries"
patterns = ["/home/*/.rustup/toolchains"]

[[redownload]]
hidden = true
patterns = ["/home/*/.rustup/downloads", "/home/*/.rustup/tmp"]

[[redownload]]
label = "Cargo crate cache"
how = "cargo downloads crates again when you build"
patterns = ["/home/*/.cargo/registry", "/home/*/.cargo/git"]
```

## Settings

| Setting | Required | What it is |
|---|---|---|
| `id` | yes | Lowercase letters, digits and dashes. Must match the file name. |
| `name` | yes | The app's name as users know it. |
| `excludes` | no | Patterns to leave out that need no explanation (caches). `not-backed-up.md` lists what they match under "Caches and other regenerated data". |
| `[recognize]` | yes | How to tell the app is there. At least one entry. |
| `recognize.packages` | | Package names. |
| `recognize.flatpaks` | | Flatpak app ids. |
| `recognize.paths` | | Settings folders, relative to the home folder (`.config/discord`, not `~/.config/discord`). |
| `[[redownload]]` | no | Something left out that comes back by itself. One block per kind of thing. |
| `redownload.patterns` | yes | The patterns it covers. They are left out, so don't repeat them in `excludes`. |
| `redownload.label` | yes | What it is, for `not-backed-up.md`: "Steam shader caches". |
| `redownload.how` | yes | How it comes back, as the end of a sentence without the final period: "Steam downloads it on first start". |
| `redownload.details` | no | What else to show: `versions` (the default: version numbers in the folder names), `entries` (the folders' contents, such as toolchain names), `steam-games` (each installed game, from Steam's own manifests) or `none`. |
| `redownload.hidden` | no | `true` to leave it out without listing it (half-finished downloads). Then `label` and `how` aren't needed. |
| `[[note]]` | no | A message shown after a restore. |
| `note.text` | yes | The message. Say what happened and what to do. |
| `note.paths` | no | Show it when one of these (relative to the home folder) is restored. Without it: when any of `recognize.paths` is. |

Names in `[recognize]` and `note.paths` may use the wildcards `*`, `?` and `[...]` (`electron*`).
Unknown settings are errors, so a typo can't go unnoticed.

TOML puts every key that follows a `[table]` header inside that table, so `id`, `name` and
`excludes` must come **before** `[recognize]`.

## Patterns

`excludes` and `redownload.patterns` use **restic's** pattern rules, the same as `excludes.txt`:

- `*` matches within one folder name; `**` matches any number of folders: `/home/*/.config/**/Cache`.
- A pattern matches a file or folder, and a matching folder is left out with everything in it.
- `?`, `[abc]`, `[a-z]` and `[^a]` work as usual; a backslash makes the next character literal.

Catalog patterns must also:

- start with `/`, so they can't match something unrelated deeper down (patterns without a leading
  `/` match at any depth);
- not start with `!` (taking back a match depends on the order of the whole list, which the catalog
  doesn't control);
- not use variables like `$HOME`: catalog files are the same on every machine. Write
  `/home/*/...` for every user's home folder.

## What happens with an entry

- Every app's patterns join the exclude list on every backup, whether or not the app is installed
  (a pattern that matches nothing costs nothing). The user can switch any of them off, like a
  default, in the config.
- After each backup, `not-backed-up.md` in the record lists what the patterns left out of each home
  folder, grouped by `label`, largest first, with its size and `how`.
- After a restore, the notes whose paths were restored are shown.
- `[recognize]` ties packages, Flatpaks and settings folders to the app, for grouping a restore by
  app.

`go test ./internal/catalog` loads every file here and checks it.
