# desktop: system record

User `alice`, kernel 6.17.1-arch1-1, desktop chassis. Complete.

Restore offers each bundle; its *when* rule decides whether it starts ticked on the target machine.

## Packages from the official repos
`packages` · 2 items · when: `always`

- package    base  [core]
- package    fuse3  [extra]  (as dependency)

## AUR packages (rebuilt with paru)
`aur` · 1 items · when: `always`

- AUR        paru-bin

## Extra package repositories
`repos` · 1 items · when: `always`

- repo       [myrepo] + signing key (Example Repo <repo@example.com>)

## Enabled system services
`services` · 1 items · when: `always`

- enable     sshd.service

## Enabled user services
`user-services` · 3 items · when: `always`

- user unit  enable rclone-mount.service
- user unit  rclone-mount.service
- folder     ~/Cloud

## Tweaked package settings
`configs` · 1 items · when: `always`

- file       /etc/ssh/sshd_config  (edited package file)

## Other hand-installed system files
`other-files` · 1 items · when: `always`

- file       /usr/local/bin/hello

## Kernel parameters
`kparams` · 1 items · when: `always`

- kernel     quiet

## Locale, keyboard & timezone
`locale` · 1 items · when: `always`

- file       /etc/locale.conf

## Group memberships & login shell
`account` · 2 items · when: `always`

- group      wheel
- shell      /usr/bin/zsh

## Language tools (cargo, pipx, npm, flatpak)
`dev-tools` · 1 items · when: `always`

- flatpak    org.example.App

## Machine identity: hostname, SSH host keys, Tailscale node
`identity` · 1 items · when: `samehost`

- backup     /etc/ssh/ssh_host_*

## root's home directory
`root-home` · 1 items · when: `samehost`

- backup     /root

## Home: app settings & data (dotfiles)
`home-apps` · 1 items · when: `always`

- backup     ~/.config/kitty

## Deliberately not in this record
Secrets live only in the encrypted backup: Wi-Fi/VPN passwords, SSH host keys, /etc/shadow, Bluetooth link keys, Tailscale state, /root and your home directory.
