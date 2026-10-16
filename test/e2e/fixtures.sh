# What machine A is set up with, and so what machine B must have after the restore. Sourced.

# Packages installed from the repositories on A (none from the AUR: see README.md).
APP_PACKAGES="openssh zsh tree htop"
# Tools the test itself needs on both machines; Carryon needs restic, and git for the record's history.
TOOL_PACKAGES="restic git jq diffutils"
# A unit to switch on, from one of APP_PACKAGES.
SERVICE=sshd.service
# System files the test changes or adds, compared by content, mode and owner.
SYSTEM_FILES="/etc/ssh/sshd_config /etc/exampled/exampled.conf /etc/exampled/secret.key /opt/exampledata/data.txt"
# Home files compared by content, mode and owner, relative to each home.
HOME_FILES=".zshrc notes/todo.txt .config/exampleapp/settings.ini .config/exampleapp/themes/dark.ini"
# Users, their extra groups and login shells.
USERS="alice bob"
declare -A GROUPS_OF=([alice]="wheel,video" [bob]="audio")
declare -A SHELL_OF=([alice]="/usr/bin/zsh" [bob]="/bin/bash")
