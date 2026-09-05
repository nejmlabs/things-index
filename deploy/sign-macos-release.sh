#!/bin/bash
# Sign only on an isolated macOS release runner. Never import into an existing
# keychain or change certificate trust. See docs/macos-signing.md.
set +x # security(1) requires password arguments; never trace those commands.
set -euo pipefail
umask 077

fail() { printf 'macOS release signing: %s\n' "$1" >&2; exit 1; }

[[ $# -eq 1 ]] || fail 'usage: sign-macos-release.sh UNIVERSAL_BINARY'
[[ "$(uname -s)" == Darwin ]] || fail 'requires macOS'
[[ -n "${MACOS_SIGNING_CERTIFICATE_P12_BASE64:-}" ]] || fail 'MACOS_SIGNING_CERTIFICATE_P12_BASE64 is required'
[[ -n "${MACOS_SIGNING_CERTIFICATE_PASSWORD:-}" ]] || fail 'MACOS_SIGNING_CERTIFICATE_PASSWORD is required'
[[ "${MACOS_SIGNING_IDENTITY_SHA1:-}" =~ ^[[:xdigit:]]{40}$ ]] || fail 'MACOS_SIGNING_IDENTITY_SHA1 must be the 40 hexadecimal digit certificate fingerprint'
[[ -f "$1" && ! -L "$1" ]] || fail 'expected a regular universal binary, not a symlink'

artifact="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
identifier=com.nejmlabs.things-index
identity_sha1="$MACOS_SIGNING_IDENTITY_SHA1"
p12_password="$MACOS_SIGNING_CERTIFICATE_PASSWORD"
unset MACOS_SIGNING_CERTIFICATE_PASSWORD MACOS_SIGNING_IDENTITY_SHA1
architectures="$(/usr/bin/lipo -archs "$artifact")"
case "$architectures" in
  'arm64 x86_64'|'x86_64 arm64') ;;
  *) fail 'release binary must contain exactly arm64 and x86_64' ;;
esac

work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/thingsindex-release-signing.XXXXXX")"
# security(1) reports canonical keychain paths. Resolve macOS aliases such as
# /var -> /private/var before adding our keychain; preserve original entries.
work_dir="$(cd "$work_dir" && pwd -P)"
keychain="$work_dir/release.keychain-db"
search_list_saved=false
keychain_creation_attempted=false

# JSON preserves spaces and other special characters in the original paths.
# No shell evaluation of security(1) output is needed.
keychain_search_list() {
  python3 - "$1" "$work_dir/search-list.json" "$keychain" <<'PY'
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys

action, saved_path, keychain = sys.argv[1:]
command = ["/usr/bin/security", "list-keychains", "-d", "user"]

def current():
    result = subprocess.run(command, check=True, capture_output=True, text=True, timeout=20)
    paths = shlex.split(result.stdout.strip() or result.stderr.strip())
    if any(not os.path.isabs(path) for path in paths):
        raise RuntimeError("Unexpected keychain search-list output")
    return paths

if action == "save":
    Path(saved_path).write_text(json.dumps(current()))
else:
    original = json.loads(Path(saved_path).read_text())
    paths = [keychain, *original] if action == "add" else original
    subprocess.run([*command, "-s", *paths], check=True, capture_output=True, timeout=20)
    if current() != paths:
        raise RuntimeError("Keychain search list did not match the requested list")
PY
}

cleanup() {
  status=$?
  trap - EXIT
  trap '' INT TERM HUP
  if [[ "$keychain_creation_attempted" == true ]]; then
    if ! /usr/bin/security delete-keychain "$keychain" > /dev/null 2>&1; then
      printf 'macOS release signing: temporary keychain cleanup failed\n' >&2
      status=1
    fi
  fi
  if [[ "$search_list_saved" == true ]]; then
    if ! keychain_search_list restore > /dev/null 2>&1; then
      printf 'macOS release signing: original keychain search list could not be restored\n' >&2
      status=1
    fi
  fi
  if ! rm -rf -- "$work_dir"; then
    printf 'macOS release signing: temporary file cleanup failed\n' >&2
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# Output from keychain operations can contain private key attributes. Capture
# it only in the private temporary directory and report a non-secret label.
private_step() {
  local label="$1"
  shift
  if ! "$@" > "$work_dir/security-output" 2>&1; then
    fail "$label failed; no unsigned or ad-hoc fallback is allowed"
  fi
}

if ! python3 - "$work_dir/identity.p12" <<'PY'
import base64
import os
from pathlib import Path
import sys

try:
    encoded = "".join(os.environ["MACOS_SIGNING_CERTIFICATE_P12_BASE64"].split())
    decoded = base64.b64decode(encoded, validate=True)
    if not decoded:
        raise ValueError("empty identity")
    Path(sys.argv[1]).write_bytes(decoded)
except Exception:
    sys.exit(1)
PY
then
  fail 'could not decode the PKCS#12 signing secret'
fi
unset MACOS_SIGNING_CERTIFICATE_P12_BASE64

keychain_search_list save
search_list_saved=true
keychain_password="$(python3 -c 'import secrets; print(secrets.token_urlsafe(36))')"
keychain_creation_attempted=true
private_step 'Create temporary keychain' /usr/bin/security create-keychain -p "$keychain_password" "$keychain"
private_step 'Unlock temporary keychain' /usr/bin/security unlock-keychain -p "$keychain_password" "$keychain"
keychain_search_list add
private_step 'Import signing identity' /usr/bin/security import "$work_dir/identity.p12" \
  -k "$keychain" -f pkcs12 -P "$p12_password" -T /usr/bin/codesign -x
unset p12_password
# Restrict the imported private key to codesign, not all applications (-A).
# Apple's key partition check also requires apple: for /usr/bin/codesign.
private_step 'Set signing key partition' /usr/bin/security set-key-partition-list \
  -S apple: -s -t private -k "$keychain_password" "$keychain"
unset keychain_password

private_step 'Sign universal executable' /usr/bin/codesign --force \
  --sign "$identity_sha1" --keychain "$keychain" --identifier "$identifier" \
  --timestamp=none "$artifact"

# The fixed identifier and exact leaf certificate must match on BOTH slices.
# Signing without --requirements lets codesign generate the certificate-based
# designated requirement. A code-hash requirement would change every build.
requirement="=identifier \"$identifier\" and certificate leaf = H\"$identity_sha1\""
for architecture in arm64 x86_64; do
  /usr/bin/codesign --verify --strict --architecture "$architecture" \
    --test-requirement "$requirement" "$artifact"
  /usr/bin/codesign --display --verbose=4 --architecture "$architecture" -r- \
    "$artifact" > "$work_dir/$architecture.signature" 2>&1
  [[ "$(sed -n 's/^Identifier=//p' "$work_dir/$architecture.signature")" == "$identifier" ]] \
    || fail "unexpected identifier for $architecture"
  sed -n 's/^designated => //p' "$work_dir/$architecture.signature" > "$work_dir/$architecture.requirement"
  [[ "$(wc -l < "$work_dir/$architecture.requirement" | tr -d ' ')" == 1 ]] \
    || fail "missing or ambiguous designated requirement for $architecture"
  if grep -qi cdhash "$work_dir/$architecture.requirement"; then
    fail "designated requirement depends on a code hash for $architecture"
  fi
done
cmp -s "$work_dir/arm64.requirement" "$work_dir/x86_64.requirement" \
  || fail 'architectures have different designated requirements'
/usr/bin/codesign --verify --strict --all-architectures "$artifact"
printf 'Signed and verified arm64 and x86_64 as %s.\n' "$identifier"
