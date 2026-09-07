# Stable macOS release signing

**Installing or updating your Mac?** Follow [Mac worker setup](homelab.md#mac-worker)
and the [one-time heading permissions](../shortcuts/README.md#first-run-verification).
You do not need to create a certificate, install a private key, or configure
GitHub secrets. The release-maintainer details below explain the signing setup.

The macOS release workflow signs the final universal executable with one
persistent certificate and the identifier `com.nejmlabs.things-index`. It then
verifies both `arm64` and `x86_64` against that identifier and certificate before
GitHub attests and uploads the file. Tagged releases and manual dry runs both
require signing; missing or invalid configuration fails the build.

The installed file remains byte-for-byte the signed, attested release asset.
The worker does not need a private signing key or access to a signing keychain.
Local `make dist-mac` builds do not run this release signing step.

The worker installer is a shell wrapper around the Go application and uses
macOS's built-in `codesign` to check the public release certificate before
executing a download. It does not require Python or Xcode Command Line Tools.
The trusted Go installer then verifies both architectures and continuity with
any installed signing certificate, preserves a private binary backup, stops
the worker, and installs the new executable before running its setup wizard.
`--no-setup` leaves the worker stopped until setup is run interactively.

The public certificate fingerprint is pinned in `deploy/mac-worker-install.sh`.
The release workflow checks that its configured signing identity matches this
pin, so a changed CI secret cannot silently publish an incompatible installer.

The updater checks each architecture before executing or installing a download.
Every slice must use the same signing certificate, and a signed installation
only accepts that same certificate with compatible designated requirements.
This deliberately makes certificate rotation an attended migration. Missing
signatures, ad hoc candidates, changed certificates, and incomplete universal
binaries leave the installed worker untouched.

## Required GitHub Actions secrets

Configure these repository secrets before running the release workflow:

| Secret | Value |
| --- | --- |
| `MACOS_SIGNING_CERTIFICATE_P12_BASE64` | Base64 of a password-protected PKCS#12 file containing the persistent code-signing certificate and its private key. Wrapped base64 is accepted. |
| `MACOS_SIGNING_CERTIFICATE_PASSWORD` | The nonempty password used to export that PKCS#12 file. |
| `MACOS_SIGNING_IDENTITY_SHA1` | The signing certificate's SHA-1 fingerprint: exactly 40 hexadecimal characters, without spaces or colons. This selects and verifies the certificate; it is not the executable's file hash. |

Provision the production identity separately, with an explicit owner. A
self-signed code-signing certificate is sufficient for this stable-identity
scheme and does not require Apple Developer membership. Its certificate must
permit code signing, and the PKCS#12 must include the matching private key.
Do not generate a fresh certificate in CI, or regenerate one for each release.
The signing script does not create certificates or upload secrets.

Retain the **same certificate and private key** across builds. Keep an encrypted
backup outside GitHub and store its password separately; GitHub secrets are not
a recoverable backup. Record the public certificate, fingerprint, expiration
date, and responsible owner. Monitor expiry and plan renewal or compromise
recovery before changing the identity. A replacement or reissued certificate
can change the designated requirement even if its name is unchanged. Treat
rotation as an attended permission migration, not an ordinary version update.
Changing the release certificate also requires deliberately updating the
installer's public pin and validating the new installation route.
Do not assume self-signed certificates have Developer ID timestamp or
revocation behavior.

## Runner isolation and verification

`deploy/sign-macos-release.sh UNIVERSAL_BINARY` is intended for the isolated
macOS GitHub runner. It accepts only a universal executable containing exactly
`arm64` and `x86_64`. The script:

1. Decodes the identity into a private temporary directory under `RUNNER_TEMP`
   (or `TMPDIR` for a local disposable test).
2. Creates a uniquely named temporary keychain and saves the existing user
   keychain search list. It does not change the default keychain.
3. Imports the private key as nonextractable, with access restricted to
   `/usr/bin/codesign` and the required `apple:` key partition. It does not use
   the allow-all-applications `-A` option.
4. Signs the already assembled universal file using the configured fingerprint,
   fixed identifier, and `--timestamp=none`.
5. Verifies the signature, certificate, and identifier on each architecture,
   requires matching designated requirements, and rejects code-hash-based
   requirements that would change with every build.
6. Deletes the temporary keychain and identity files and restores the original
   search list in an exit/signal trap. Cleanup failure fails the signing step.

The script never adds a trusted root or changes certificate trust settings.
Password-bearing commands are not traced, and their output is not printed.
Apple's `security` CLI does require passwords as process arguments, so run this
only on an isolated runner, not a shared interactive host. Normal failures and
termination signals trigger cleanup; force-killing a process or losing a runner
cannot run a shell trap. GitHub's disposable runner provides the final isolation
boundary.

Run a manual Release workflow first and inspect its successful signing,
verification, and attestation steps before tagging a production release. The
uploaded artifact is suitable for a controlled migration and update test. No
production certificate or GitHub secrets are provisioned by this documentation.

## Initial Full Disk Access migration

Stable signing supplies a consistent code identity; it does not grant macOS
permissions. Existing ad-hoc builds have a different identity. The first move
to the persistent certificate requires an attended migration on the Mac.
`~/.local/bin/things-index worker --setup` guides this process: it checks the signing
identity, stops the worker for the permission step, opens Full Disk Access
settings, and reveals the resolved executable. It verifies a readable stored
grant against that executable. If macOS prevents that inspection, it requires
explicit manual confirmation and reports that the stored grant is unverified.
It never edits TCC or grants access automatically.

The migration steps are:

1. Install and verify the signed release at the actual launch agent executable
   path (normally `~/.local/bin/things-index`). Stop the worker while updating
   its permission entry.
2. In System Settings → Privacy & Security → Full Disk Access, **remove the old
   ThingsIndex executable entry and add the newly signed executable**. Toggling
   an old entry alone can retain a stale code requirement. Enable the new entry.
3. Let setup re-run the worker's Automation preflight; an existing successful
   preflight marker does not validate the new identity. Approve any required
   Automation prompt. Setup checks fresh database access, the harmless Things
   list-count Apple Event, and worker readiness, then repeats the checks after
   a controlled restart. Failure leaves the worker stopped and setup incomplete.
4. Confirm database access and worker health after a restart, then install a
   **different build signed with the same certificate** and repeat the checks
   without interacting with the Mac's screen. Include a login/reboot check for
   the intended unattended operating conditions.

Apple documents that Full Disk Access avoids additional AppData prompts for
protected app data; without it, an AppData consent can last only until the
accessing process quits. Stable code identity is needed to associate permission
with subsequent builds. [Apple WWDC23: What's new in privacy](https://developer.apple.com/videos/play/wwdc2023/10053/),
[Apple DTS: On File System Permissions](https://developer.apple.com/forums/thread/678819)

Full Disk Access and Automation are separate permissions. Signing does not
grant Things control, accept Shortcuts prompts, or promise that future macOS or
Things changes can never require consent. Local integration validation signed
two different universal ThingsIndex builds through the release script and ran
their `version` command without adding certificate trust. The real updater
guard accepted a same-certificate update and legacy migration, and rejected a
changed certificate, an uncertified candidate, and a missing architecture.
The v0.2.6 Mac mini validation preserved stored Full Disk Access and Automation
records across signed updates and worker restarts. After separate attended
Shortcut action approvals, background heading create/rename/archive also passed
without interaction. These are results on the tested Mac, not a guarantee that
future OS or Things changes cannot require approval. [Apple: Allow apps to control other apps](https://support.apple.com/en-gb/guide/mac-help/mchl108e1718/mac)

## Self-signing is not notarization

This setup provides a persistent local code identity. It does not provide an
Apple Developer ID certificate, notarization, or automatic Gatekeeper trust.
The workflow does not alter that trust boundary or disable quarantine checks.
The installers use `curl`, and the updater writes the HTTP response directly
to a fresh staging file. They do not remove quarantine attributes or change
Gatekeeper policy. Fresh private staging avoids inheriting metadata from an
earlier download. Browser downloads may be quarantined and blocked, so verify
the actual installation route on the target Mac before unattended deployment.
[Apple DTS: Resolving Trusted Execution Problems](https://developer.apple.com/forums/thread/706442)

If public distribution later needs those properties, treat Developer ID and
notarization as a separate release change with an explicit identity migration.
[Apple: Code Signing Guide](https://developer.apple.com/library/archive/documentation/Security/Conceptual/CodeSigningGuide/AboutCS/AboutCS.html)
