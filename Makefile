# Builds and installs Carryon. Every package (the AUR PKGBUILDs, the release tarball) uses these
# targets, so the install layout lives in one place: docs/design.md, "Install layout".
#
#   make                                  build the binary, man page and completions into build/
#   sudo make install                     install them (PREFIX=/usr, DESTDIR= for packages)
#   make dist GOARCH=arm64                a release tarball for another architecture
#   make check-install                    install into a temporary folder and check the layout
#
# The units, the pacman hook and the binary's built-in paths expect PREFIX=/usr. Nothing under /etc
# or /var is installed: `carryon init` creates those.

PREFIX  ?= /usr
DESTDIR ?=
BUILD   ?= build

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || true)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || true)
# The commit's time, so the same commit always builds the same files (makepkg sets its own).
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || date +%s)
export SOURCE_DATE_EPOCH
DATE    ?= $(shell date -u -d "@$(SOURCE_DATE_EPOCH)" +%Y-%m-%dT%H:%M:%SZ)
ifeq ($(VERSION),)
VERSION := dev
endif

GO        ?= go
GOARCH    ?= $(shell $(GO) env GOARCH)
# Extra linker flags, such as "-s -w" for the release. Not LDFLAGS: makepkg sets that one for C.
GO_LDFLAGS ?=

PKG   := github.com/Evimore/carryon/internal/version
XLDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE) $(GO_LDFLAGS)

ARCH_amd64 := x86_64
ARCH_arm64 := aarch64
ARCH       := $(or $(ARCH_$(GOARCH)),$(GOARCH))
DISTNAME   := carryon-$(VERSION)-linux-$(ARCH)

BINDIR     := $(DESTDIR)$(PREFIX)/bin
UNITDIR    := $(DESTDIR)$(PREFIX)/lib/systemd/system
HOOKDIR    := $(DESTDIR)$(PREFIX)/share/libalpm/hooks
SHAREDIR   := $(DESTDIR)$(PREFIX)/share/carryon
MANDIR     := $(DESTDIR)$(PREFIX)/share/man/man1
BASHDIR    := $(DESTDIR)$(PREFIX)/share/bash-completion/completions
ZSHDIR     := $(DESTDIR)$(PREFIX)/share/zsh/site-functions
FISHDIR    := $(DESTDIR)$(PREFIX)/share/fish/vendor_completions.d
LICENSEDIR := $(DESTDIR)$(PREFIX)/share/licenses/carryon
DOCDIR     := $(DESTDIR)$(PREFIX)/share/doc/carryon

.PHONY: all build docs install uninstall check-install dist clean

all: build docs

# A static binary (CGO_ENABLED=0), so it runs on any distribution and from the recovery kit.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags '$(XLDFLAGS)' -o $(BUILD)/carryon ./cmd/carryon

# Generated on the build machine, whatever GOARCH the binary is for.
docs:
	env -u GOOS -u GOARCH CGO_ENABLED=0 $(GO) run ./internal/tools/gendocs -out $(BUILD) -version $(VERSION)

# Installs what `make` built; it never compiles, so `sudo make install` doesn't run Go as root.
install:
	@test -x $(BUILD)/carryon -a -f $(BUILD)/man/carryon.1 || { echo "Nothing to install yet: run make first." >&2; exit 1; }
	install -Dm755 $(BUILD)/carryon -t $(BINDIR)
	install -Dm644 dist/systemd/*.service dist/systemd/*.timer dist/systemd/*.path -t $(UNITDIR)
	install -Dm644 dist/pacman/carryon.hook -t $(HOOKDIR)
	install -Dm644 catalog/excludes.default catalog/system-folders.txt -t $(SHAREDIR)
	install -Dm644 catalog/*.toml -t $(SHAREDIR)/catalog
	install -Dm644 $(BUILD)/man/carryon.1 -t $(MANDIR)
	install -Dm644 $(BUILD)/completions/carryon.bash $(BASHDIR)/carryon
	install -Dm644 $(BUILD)/completions/_carryon -t $(ZSHDIR)
	install -Dm644 $(BUILD)/completions/carryon.fish -t $(FISHDIR)
	install -Dm644 LICENSE -t $(LICENSEDIR)
	install -Dm644 README.md docs/design.md -t $(DOCDIR)
	install -Dm644 catalog/README.md $(DOCDIR)/catalog.md

uninstall:
	rm -f $(BINDIR)/carryon $(HOOKDIR)/carryon.hook $(MANDIR)/carryon.1
	rm -f $(addprefix $(UNITDIR)/,$(notdir $(wildcard dist/systemd/*)))
	rm -f $(BASHDIR)/carryon $(ZSHDIR)/_carryon $(FISHDIR)/carryon.fish
	rm -rf $(SHAREDIR) $(LICENSEDIR) $(DOCDIR)

# Installs into a temporary folder and compares it with packaging/layout.txt.
check-install: all
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	$(MAKE) --no-print-directory install DESTDIR="$$tmp" >/dev/null && \
	(cd "$$tmp" && find . -type f -o -type l) | sed 's|^\./||' | packaging/check-layout.sh

# The release tarball: everything `make install` installs, under DISTNAME/.
dist: all
	rm -rf $(BUILD)/$(DISTNAME) $(BUILD)/$(DISTNAME).tar.gz
	$(MAKE) --no-print-directory install DESTDIR=$(CURDIR)/$(BUILD)/$(DISTNAME) PREFIX=/usr
	tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$(SOURCE_DATE_EPOCH)" \
		-C $(BUILD) -czf $(BUILD)/$(DISTNAME).tar.gz $(DISTNAME)
	@echo $(BUILD)/$(DISTNAME).tar.gz

clean:
	rm -rf $(BUILD)
