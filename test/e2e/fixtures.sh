# What machine A is set up with, and so what machine B must have after the restore. Sourced.

# Packages installed from the repositories on A (none from the AUR: see README.md).
APP_PACKAGES="openssh zsh tree htop nano"
# Tools the test itself needs on both machines; Carryon needs restic, and git for the record's history.
TOOL_PACKAGES="restic git jq diffutils"
# A unit to switch on, from one of APP_PACKAGES.
SERVICE=sshd.service
# System files the test changes or adds, compared by content, mode and owner. DATA_DIR's file is
# backed up only once the inbox's suggestion for it is accepted.
SYSTEM_FILES="/etc/ssh/sshd_config /etc/exampled/exampled.conf /etc/exampled/secret.key /opt/exampledata/data.txt /var/lib/exampledb/data.db"
# Home files compared by content, mode and owner, relative to each home.
HOME_FILES=".zshrc notes/todo.txt .config/exampleapp/settings.ini .config/exampleapp/themes/dark.ini .config/nano/nanorc"
# Users, their extra groups and login shells.
USERS="alice bob"
declare -A GROUPS_OF=([alice]="wheel,video" [bob]="audio")
declare -A SHELL_OF=([alice]="/usr/bin/zsh" [bob]="/bin/bash")
# A folder in /var/lib holding data no package owns: the backup suggests it in the inbox.
DATA_DIR=/var/lib/exampledb
# An app whose bundle has saved settings (each user's ~/.config/nano), for restoring as you go.
# Machine C installs it after connecting.
APP=nano
APP_BUNDLE=app-nano
# A user unit alice enables, with lingering so her units run while she's signed out.
UNIT_USER=alice
USER_UNIT=example-sync.service
