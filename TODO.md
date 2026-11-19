# TODO

## Measurement report for delta updates (do before designing anything)
Compare two real builds with tools already installed (no new tools without consent):
- Build `lanmsg-server` for `linux/arm` at 0.1.2 and 0.1.3 into a scratch dir; also the GUI.
- Report: file size, ELF section sizes, Go build ID, count of differing bytes (`cmp -l`),
  xz and zstd compressed sizes, and `zstd --patch-from` delta size.
- Decide from the numbers whether deltas are worth it.

## Next steps for the update system (after the report)
1. Delta computed on the build host and listed in the signed manifest
   (`deltas`: base sha256, patch sha256, size). The router only serves static files.
2. Client applies the patch to its own running binary, verifies the result against the
   signed full-file sha256, and falls back to the full download on any failure.
3. The router never holds the release key and never decides what is trusted.

## Needs a relay (central server) change, so a coordinated relay + client release (added 2026-11-19)
- **Resume proves the device key.** Today a resuming device proves only the household passphrase; add a relay-issued
  challenge signed by the device key. Old clients would stop connecting, so ship relay and clients together.
- **Layer 5: per-connection admin challenge.** The admin key signs a relay-issued challenge (bound to the TLS session)
  instead of a timestamp. Replaces the replay cache for admin frames.
- **Tunnel entrance label.** Second public port on the AWS tunnel hub (e.g. 8445 = `ssh-forward`, 8444 = `direct-cli`),
  label carried in the stream preamble to the relay, stored as `tunnel/<label>` in the path. Needs the AWS hub
  redeployed and the Android profile pointed at the new port. THEN show the label in Properties and the roster line.
  (A label separates entrances, not individual SSH users; per-user ports would be needed for that.)
- Android: send client kind/OS/arch in its hello and the model in signed facts (AndroidSSH project).

## Revoke / return alerts: follow-ups (2026-11-19)
- Fingerprint comparison cannot use the signed facts (build hash, model) of a NEW pending registration: facts are sent only
  after the device is ready. Consider sending them while pending too, so a lookalike can be compared by build hash.
- A client refused as "forbidden" (revoked) stops retrying until restarted; after Re-authorize + approve the person must
  restart their app. Consider a slow retry (e.g. every 10 minutes) for that case.
- If `require_admin_approval` is off in the router's config, a lookalike with a new key becomes active at once (only the
  alert fires). Check `/mnt/ssd/lanmsg/etc/server.toml` before relying on this.

## Other open items
- Sign and stage GUI 0.1.3 (`lanmsg-signrelease sign ... -out-dir dist/signed6`).
- Stray untracked files: `New file`, `QUICKREF.md`, `capture_names.json`.
- Device resume does not prove possession of the device key (passphrase only); consider a signed challenge.

## Discussion: relay admin console / web page / update interface (raised 2026-11-18)
- Today the relay takes no runtime commands: only `lanmsg-server` subcommands run locally (restart needed
  for config changes), signed admin frames, and read-only `/updates/*`. Updates are staged by tar over ssh.
- Recommendation: no admin web page by default (the tunnel exposes the same HTTP mux to the internet).
  Build first: `lanmsg-cli admin devices|stats|remove|reauth`, `lanmsg-server stats` (local DB read),
  and a signed `admin_upload_update` frame (admin key only; release key never on the router).
- Optional later: separate off-by-default LAN/loopback-only read-only stats page, one self-contained file.

## Admin-key hardening: this computer only (raised 2026-11-18)
Decision so far: admin must never be possible over the AWS Tor box; the relay must enforce it (client-side
checks are not security).
- Layer 1 (DONE 2026-11-18): relay rejects every admin frame not arriving on the `lan` path, even with a valid
  signature. Consequence: approve/remove/re-auth become LAN-only. NEEDS the user's yes before building.
- Layer 4 (planned): compile admin code out of `lanmsg-remote-cli` (build tag).
- Layer 5 (planned): per-connection challenge signed by the admin key (channel binding) instead of a timestamp.
- Optional: pin admin to this machine's LAN address (weak; addresses can be spoofed).
- Legacy gap: until an admin key is installed on the relay, a device-flagged admin can still approve/deny
  over the tunnel. Install the admin key first.

## TPM (Trusted Platform Module) for the admin key: research later
- Goal: create the admin key inside a TPM so it can never be copied off this machine.
- First find out, with read-only checks and the user's go-ahead: does this computer have a TPM
  (`/sys/class/tpm`, `/dev/tpm0`, `/dev/tpmrm0`, firmware setting), is it 1.2 or 2.0, and which other
  devices the user owns might have one (other PCs, laptops, phone secure element, router?).
- Then compare: TPM 2.0 on this box vs a FIDO2 hardware key (USB token) vs the current passphrase file.
- Tools likely needed (`tpm2-tools` or a Go TPM library) are NOT installed; installing needs explicit consent.
- Discuss in a later session before any code.

## Discussion: client rollback / signed downgrade (raised 2026-11-18; user wants a fuller explanation first)
- Today clients refuse anything not newer: manifest `Seq` must rise (anti-replay) and `CheckSelf` only offers a
  strictly newer version. Relay rollback is manual (`lanmsg-server.prev`), by design.
- Idea: a rollback is published as a NEW higher-Seq manifest whose entry is the older binary, with a signed
  `downgrade` flag (and a `min_compatible` version); only the release key can trigger it; `lanmsg-signrelease`
  needs an explicit `--allow-downgrade`.
- Open choice: general downgrade vs only "emergency withdraw" (stop new installs, move nobody backward).
- Cautions: local database migrations across versions; Android refuses lower version codes; per-target flag.
- Come back to this when working through the TODO list, with a plain-language walkthrough first.

## NEXT TASK (set by the user 2026-11-18, after GUI 0.1.4 is staged): command presentation
- Every command shown back in a reply's command section: (1) each command name is a link to a man page
  describing it, (2) shell syntax highlighting (fenced ```bash blocks).
- To clarify with the user first: "command section" = the command blocks in Claude's replies, or a section
  of one of their apps/docs? Man page source to use (e.g. manpages.debian.org) and whether links are checked.

## Per-recipient display names (option C), to discuss later (raised 2026-11-19)
The owner would like to present a different display name to each person. Today the relay holds ONE display name per device
and every client resolves senders by device ID, so this needs per-viewer names in the relay's directory (and a signed way to
set them), or a local-only nickname layer in each client. Much more work than the admin rename (option B), which is the
version being built first.
