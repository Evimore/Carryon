#!/bin/bash
# Installs Carryon from the checkout mounted at /src, where docs/design.md "Install layout" puts it.
# The binary is built on the host (bin/carryon). T19's package will replace this step.
. /src/test/e2e/lib.sh

step "Installing Carryon"
install -Dm755 /src/test/e2e/bin/carryon /usr/bin/carryon
install -Dm644 -t /usr/lib/systemd/system /src/dist/systemd/*
install -Dm644 -t /usr/lib/systemd/user /src/dist/systemd-user/*
install -Dm644 /src/dist/pacman/carryon.hook /usr/share/libalpm/hooks/carryon.hook
install -Dm644 -t /usr/share/carryon /src/catalog/excludes.default /src/catalog/system-folders.txt
install -d /usr/share/carryon/catalog
install -m644 /src/catalog/*.toml /usr/share/carryon/catalog/
systemctl daemon-reload
carryon version
