#!/bin/sh
set -eu

case "$(uname -m)" in
  x86_64) native_sha=bcebc898d22419417b8dbec0f5e3f00f5b67b5a38c1de37c77c699d0e31fe0dd ;;
  aarch64) native_sha=e09c47471c6987a89dddacc7bcbe44d0f3b3f2a30b7e1d20e47ae09517d29990 ;;
  *) echo "unsupported browser-canary architecture: $(uname -m)" >&2; exit 1 ;;
esac

test -x /usr/local/bin/opencode
printf '%s  %s\n' "$native_sha" /usr/local/bin/opencode | sha256sum --check --strict
test "$(/usr/local/bin/opencode --version)" = "1.18.13"

if [ "${1:-}" = "--verify-native" ]; then
  exit 0
fi

rm -f /canary/evidence/browser-escape /canary/evidence/exec.log /canary/evidence/test.log /canary/evidence/launchers
set +e
ACP_GO_OPENCODE_BROWSER_CANARY=1 timeout --signal=TERM --kill-after=20 210 \
  strace -f -qq -e trace=execve,execveat -o /canary/evidence/exec.log \
  /canary/browser-canary.test -test.run '^TestRealNativeBrowserContainment$' -test.v \
  >/canary/evidence/test.log 2>&1
status=$?
set -e
cat /canary/evidence/test.log
test "$status" -eq 0
test "$(grep -c '^--- PASS: TestRealNativeBrowserContainment' /canary/evidence/test.log || true)" -eq 1
! grep -q 'testing: warning: no tests to run' /canary/evidence/test.log
grep -q 'execve("/usr/local/bin/opencode"' /canary/evidence/exec.log
! grep -Eq 'execveat\([^,]+, "", .*AT_EMPTY_PATH' /canary/evidence/exec.log

sed -n \
  -e 's/.*execve("\([^"]*\)".*/\1/p' \
  -e 's/.*execveat([^,]*, "\([^"]*\)".*/\1/p' \
  /canary/evidence/exec.log \
  | grep -E '/(open|xdg-open|x-www-browser|www-browser|sensible-browser|gio|firefox|google-chrome|google-chrome-stable|chromium|chromium-browser)$' \
  >/canary/evidence/launchers || true
test -s /canary/evidence/launchers
while IFS= read -r launcher; do
  case "$launcher" in
    /canary/scratch/acp-go-opencode-browser-shim-*/open|\
    /canary/scratch/acp-go-opencode-browser-shim-*/xdg-open|\
    /canary/scratch/acp-go-opencode-browser-shim-*/x-www-browser|\
    /canary/scratch/acp-go-opencode-browser-shim-*/www-browser|\
    /canary/scratch/acp-go-opencode-browser-shim-*/sensible-browser) ;;
    *) echo "browser launcher escaped production shim: $launcher" >&2; exit 1 ;;
  esac
done </canary/evidence/launchers
test ! -e /canary/evidence/browser-escape
