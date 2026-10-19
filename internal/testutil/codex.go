// Package testutil supplies deterministic fake-provider fixtures. It is never
// imported by production admission or qualification code.
package testutil

// CodexQualificationScript simulates no-model qualification for unit/integration
// tests. It does not enforce a sandbox; the opt-in real suite proves confinement.
// Fallback handles ordinary provider calls after qualification has completed.
func CodexQualificationScript(fallback string) string {
	return `#!/bin/sh
case "$1" in
 --version) printf 'codex-cli fixture-v1\n'; exit 0;;
 sandbox)
  case "$*" in
   *deny-*|*credentials/codex-auth.json*|*/other/auth.json*|*/.codex/auth.json*|*/usr/bin/curl*) printf 'Operation not permitted\n' >&2; exit 1;;
  esac
  while [ "$1" != "--" ] && [ "$#" -gt 0 ]; do shift; done
  shift
  exec "$@";;
 --codex-run-as-apply-patch)
  path=$(printf '%s\n' "$2" | /usr/bin/sed -n 's/^\*\*\* Add File: //p')
  /usr/bin/touch "$path"; exit $?;;
 app-server)
  case "$CODEX_HOME" in
   */.restricted-local-probe-*/*)
    IFS= read -r line
    printf '{"id":0,"result":{"userAgent":"fake-codex"}}\n'
    IFS= read -r line
    IFS= read -r line
    printf '{"id":1,"result":{"threadId":"fake-thread","activePermissionProfile":{"id":"agentd-local-v1"}}}\n'
    while IFS= read -r line; do :; done
    exit 0;;
  esac;;
esac
` + fallback + "\n"
}
