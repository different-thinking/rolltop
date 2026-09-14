# File archive

Mail arrives with something in it worth keeping as a file — a voice memo
recorded on a phone and mailed to oneself is the case this was written for — and
a mailbox is a poor place to keep it. This plugin watches a folder, and copies
the attachments in what lands there onto storage you run.

Three protocols, chosen by the scheme of the address:

| Address | Protocol | Port |
| --- | --- | --- |
| `https://cloud.example.org/remote.php/dav/files/me/Recordings/` | WebDAV | 443 |
| `sftp://w0123456.kasserver.com/recordings` | SFTP over SSH | 22 |
| `smb://s0123456.kasserver.com/s0123456/recordings` | SMB / CIFS | 445 |

There is no separate protocol setting: the address already says which one it is,
and two fields that can disagree are a support question waiting to happen.

*(The plugin's id is still `webdav_archive`, which is what its tables and API
paths are named after. It kept the id when it grew the other two transports,
because an id is an identifier and renaming one costs a migration for nothing.)*

## ALL-INKL.COM

Worth stating plainly, because it is the thing most likely to be assumed wrong:
**the ALL-INKL.COM Netzlaufwerk is not a WebDAV service.** Their own support says
so, and their instructions hand out an SMB address — `\\s0123456.kasserver.com\s0123456`
on Windows, `smb://s0123456.kasserver.com/s0123456` on macOS and Linux. Pointing
a WebDAV target at it cannot work, whatever the address is spelled as.

Two ways in, and the first is the one to try:

- **`sftp://` — recommended.** The storage behind the Netzlaufwerk is the same
  storage the webspace uses, so an SSH user reaches the same files. Enable SSH in
  the KAS under *Tools → SSH-Zugang*, then use
  `sftp://w0123456.kasserver.com/your/folder` with the `ssh-w0123456` user. Port
  22 goes out from practically any hosted container.
- **`smb://` — the protocol the network drive actually speaks.** Paste the
  address from the KAS. The catch is port 445: a great many hosters and cloud
  networks block it outbound, because it is a decades-old worm vector. ALL-INKL
  offering a VPN for the network drive is a symptom of exactly that. If the Test
  button times out, this is why, and SFTP is the way in.

It is deliberately two halves. Sorting the mail is the filter engine's job:
point a `mail_filters` rule at whatever identifies the message (`from:` yourself
and `mimetype:audio/`, say) and have it move the mail into a folder. This plugin
watches that folder and takes it from there. Neither half knows about the other,
so either can be changed without the other noticing.

The transports are the same shape: `remoteStore` in `storage.go` is what the
queue, the worker and the browser talk to, and none of them knows which protocol
is behind it. Adding a fourth is a file, not a change to how any of that works.

## What it does

**Notices.** A stored-message hook runs for every mirrored message. When the
message is in a watched folder and carries an attachment whose Content-Type
matches the target's filter, a row goes onto the upload queue. That is all it
does inline with sync: one indexed query per message, and a row when there is
something to do.

**Uploads.** A worker sweeps the queue, every minute and whenever new mail has
just queued something. For each row it fetches the message the attachment
arrived in, parses the part back out of it, and writes it, creating the
directories above it as needed. One connection is opened per target per batch
rather than per upload, which for SFTP and SMB is one handshake instead of
twenty.

**Keeps trying.** A WebDAV server that is switched off, unreachable, or
rejecting the password is the normal case, not the exception. Nothing is lost to
it: the row stays queued and the retry ladder climbs from a minute to an hour,
for ten attempts — the better part of a day — before the row is marked given up
and left visible, with its last error, for someone to look at. Nothing is ever
silently dropped.

**Shows what happened.** `/files` browses the server itself, not the queue: what
is actually there, including anything put there by something else. Recordings
play in place.

## Why the bytes come from the message

Rolltop does not store attachment bodies separately. An attachment row points at
the raw `.eml` blob of the message it came in, and the download route
re-extracts the part on demand (`backend/syncer/index.go`,
`attachmentRowsForFiles`). So an upload is a fetch of the raw message plus a
re-parse, which is exactly why it does not happen in the sync hook: it is slow,
it can fail, and mail sync must not wait on either.

The host capability that fetches raw bytes (`plugins.RawMessageFetchHost`) is
implemented by the web server, which is also the host a plugin's `Start` is
handed — so the worker has it and the sync hook does not need it.

Pairing a queue row back to a MIME part follows the store's own rule
(`store.ReplaceAttachmentsForMessage`): filename plus size plus content type,
then filename plus size, then content type plus size, then the part's recorded
position — type-checked, so a message that reparsed into a different shape
fails visibly instead of filing the wrong attachment. There is deliberately no
filename-only pass: a phone names every recording `recording.m4a`, and matching
on the name alone would pair two different recordings to the same part.

## Names on the server

`path_template` decides where a file lands. Placeholders: `{yyyy}` `{mm}`
`{dd}` `{date}` `{time}` `{filename}` `{basename}` `{ext}` `{subject}`
`{from}` `{message_id}`. Every substituted value is reduced to something that
is one path segment — separators replaced, control characters dropped, dot runs
collapsed, length bounded — so a subject line with a slash in it cannot invent a
folder and nothing can climb above the configured base.

Two things are worth knowing about collisions:

- **The same bytes are not uploaded twice.** Before uploading, the queue is
  asked whether this content hash already landed for this target. If it did, the
  row is marked `duplicate` and records where the file is. A recording that
  arrives twice — a resend, a CC filed separately — is one file.
- **Different bytes never overwrite each other.** A phone sends every recording
  as `recording.m4a`, so the rendered path is often already taken. The second
  file takes a suffix from its own content hash: `recording-1f4a2b3c.m4a`. A
  file already there whose bytes *are* these bytes is not a collision — that is
  a previous attempt that uploaded and then failed to record that it had.

## Credentials

Both secrets are stored AES-256-GCM-encrypted with the installation's
`ROLLTOP_MASTER_KEY`, and neither is ever sent back to the browser — the
settings page is told only whether one is set.

- **Password** — all three transports.
- **Private key** — SFTP only, and the better credential for something that runs
  unattended. Paste the key itself; if it has a passphrase, put that in the
  password field. It is what ALL-INKL's own public-key instructions set up.

An SMB user name may carry its domain as `DOMAIN\user`, which is how a Windows
share is usually written down; it is read off the user name rather than made
into a third field.

**One limit worth knowing:** the SFTP transport does not pin host keys. There is
nowhere to keep a `known_hosts` file per user, and refusing every first
connection would mean nobody could configure a target at all. The connection is
still encrypted and the credential sent over it is only ever that target's own,
but a network attacker positioned between this server and the hoster could
impersonate the host. An install that cannot accept that should use WebDAV over
TLS, where the certificate chain is checked.

## The dial guard

The address is typed by the account holder into their own settings, and the
whole point is a server they run themselves — which on most installs is on the
same private network as Rolltop. So, unlike the remote-image fetcher, this
client dials RFC1918, ULA and loopback addresses.

What stays refused, always, is link-local — above all `169.254.169.254`, the
cloud metadata endpoint that hands out instance credentials — along with
multicast, unspecified, and the IANA special-purpose ranges no WebDAV server is
ever on. Redirects are not followed at all, because a redirect target is a host
the guard was never asked about.

The cost of that decision, stated plainly: by default an account holder can
point a target at RFC1918, at a ULA, and at loopback — including services bound
to `127.0.0.1` on the Rolltop host itself, which are often the ones with no
authentication. Browse and download return what the target answers, so a
configured target is an authenticated GET proxy into whatever it can reach. Only
the account holder's own targets are readable, and only by them, so this is a
signed-in user reaching the private network rather than an anonymous one.

On an install where the accounts are not all trusted, set
`ROLLTOP_WEBDAV_ALLOW_PRIVATE_HOSTS=0`. That promotes the guard to the stricter
one: loopback, RFC1918, ULA, shared address space and site-local all become
undialable, leaving only public addresses.

## Tables

| Table | What it holds |
| --- | --- |
| `plugin_webdav_archive_targets` | One configured destination: address, user name, encrypted password and private key, the folder watched, the Content-Type filter, and the path template. |
| `plugin_webdav_archive_uploads` | The queue and its history. Unique on `(user, target, message, attachment)`, so a refetched UID adds nothing. |

Both are per-user and cascade from `users`; removing a target takes its queue
rows with it.

## Settings

Everything is under **Settings → File archive**. `Test` asks the destination
whether it answers and whether the credentials are accepted, without waiting for
an upload to find out — which for SFTP and SMB also proves the port is open at
all.

## A note on the SMB library

`github.com/hirochachacha/go-smb2` is the pure-Go SMB2/3 client this ecosystem
uses, and its last release was March 2022. It works and it is widely deployed;
it is not actively maintained. That is a real cost of the SMB transport, and the
reason the settings page names SFTP first. `github.com/pkg/sftp`, by contrast,
is current.
