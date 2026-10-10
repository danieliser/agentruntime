# Codex restricted Docker seccomp profile

Copied unchanged from OpenAI Codex Security, Apache-2.0:
https://github.com/openai/codex-security/blob/10e7a22461329083414713c379e97a0c352647fe/docker/codex-security-seccomp.json

SHA256: `4d9767cdcdc79338c0ce172baab5042d1c5a7d847a0d73a10e308f54c44a286d`

The default action remains ERRNO. Compared with Moby's default profile, the
namespace/mount exceptions are clone, clone3, mount, mount_setattr, move_mount,
open_tree, pivot_root, setns, umount, umount2 and unshare. Use only with dropped
outer capabilities and no-new-privileges; do not enable privileged containers,
SYS_ADMIN or unconfined seccomp. The profile is embedded in the host binary and
never fetched at boot. Callers cannot select or replace it.

Codex 0.162.0 retains PID isolation when its supported fresh-proc mount fallback
is needed. No inherit-PID flag or legacy Landlock fallback is enabled.
