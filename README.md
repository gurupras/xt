# xt

Fast, resumable file transfer over ssh, with rsync's command line.

```
xt -avz --delete src/ user@host:backup/
```

xt runs `xt --server` on the remote host over your ordinary `ssh`, then syncs
with a delta algorithm like rsync's: only the changed parts of files go over
the wire. It adds:

- **Resume.** With `--partial`/`-P`, an interrupted transfer picks up where it
  stopped. The partial file is checked by hash, so only the rest is sent.
- **End-to-end integrity.** Every file is checked against a SHA-256 of the
  source. A mismatch is retried once from scratch.
- **A modern progress display.** One live line shows the whole transfer
  (percent, rate, ETA, current file), plus a short summary at the end.
- **Safe receiving.** Peer paths are validated, so `..`, absolute paths and
  symlink tricks can't write outside the destination. Files are written to a
  temp file and renamed into place.
- **Windows support.** Works on Windows hosts as well as Linux and macOS.

xt speaks its own protocol. It talks only to xt, not to rsync.

## Install

```
go install github.com/gurupras/xt@latest
```

Or build from a checkout with `go build .`, then copy the binary.

**xt must be installed and on the `PATH` of the remote host as well.** If
it's somewhere else, give its location with `--xt-path=/opt/bin/xt` (or
rsync's spelling, `--rsync-path`). xt never copies itself to the remote.

### Windows

Put `xt.exe` in a directory on the system `PATH`, for example
`C:\Program Files\xt`, so that it works from the default OpenSSH shell
(cmd.exe or PowerShell).

Windows Defender sometimes flags unsigned Go binaries as malware with an ML
heuristic (e.g. `Trojan:Win32/Bearfoos.A!ml`) and quarantines them. This is a
false positive. If it happens, add an exclusion for the install directory:

```
Add-MpPreference -ExclusionPath 'C:\Program Files\xt'
```

On a Windows receiver, a file whose name Windows can't represent is reported
and skipped; the rest of the transfer continues. Such names include ones
with `: * ? " < > | \`, a trailing dot or space, or a reserved name like
`CON` or `NUL`. Permissions from a Windows sender aren't applied on Unix.

## Usage

```
xt [OPTION...] SRC... [USER@]HOST:DEST    push
xt [OPTION...] [USER@]HOST:SRC... DEST    pull
xt [OPTION...] SRC... DEST                local copy
xt [OPTION...] SRC                        list files
```

These work the same as in rsync:
- A trailing slash on a source directory copies its contents. Without the
  slash, the directory itself is copied.
- `-a` means `-rlptgoD`.
- Files whose size and mtime match are skipped unless you pass `-c` or `-I`.

`xt --help` lists every option. The main groups:

| | |
|---|---|
| Selection | `-r -d -R --files-from -0 --exclude --include --exclude-from --include-from -f -C -m --max-size --min-size -x` |
| Comparison | `-u --existing --ignore-existing -c -I --size-only --modify-window` |
| Attributes | `-l -L -p -t -o -g -D -H -E -O --chmod` |
| Deletion | `--delete --delete-after --delete-excluded --max-delete --ignore-errors --force --remove-source-files` |
| Backups | `-b --suffix --backup-dir` |
| Data | `-z --compress-level -W --no-whole-file -S --partial -P --mkpath` |
| Output | `-v -q -i -n -h --progress --no-progress --stats --info --list-only` |
| Transport | `-e/--rsh --xt-path -4 -6 --bwlimit --timeout` |

The remote shell is `ssh` by default. You can change it with `-e` or the
`XT_RSH` environment variable, e.g. `-e 'ssh -p 2222'`.

### Unsupported rsync options

These options give an error instead of being silently ignored:
- Data and attributes: ACLs and xattrs (`-A -X`), `--inplace`, `--append`,
  `--partial-dir`, `--temp-dir`, `--link-dest` and `--copy-dest`/`--compare-dest`,
  `--chown` and `--usermap`/`--groupmap`, `--fuzzy`
- Modes and logging: the rsync daemon (`host::module`, `rsync://`), batch mode,
  `--log-file`, `--out-format`
- Copying between two remote hosts

Devices and special files are skipped.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | usage error |
| 12 | protocol or connection error (e.g. xt missing on the remote) |
| 20 | interrupted (SIGINT/SIGTERM) |
| 23 | some files could not be transferred |
| 25 | `--max-delete` stopped deletions |
| 30 | `--timeout` expired |

If the sender hits I/O errors, such as an unreadable directory, `--delete`
deletes nothing. Otherwise files might be deleted only because they couldn't
be listed. Use `--ignore-errors` to delete anyway.

## How it works

**Connection.** The client starts `ssh HOST xt --server`. Both sides then
exchange a one-line handshake, `XTPROTO <version> <os> <compression>`, and
the client skips any banner lines a noisy login shell prints first. After
that, messages are gob-encoded and optionally deflate-compressed.

**File list.** The sender walks the sources and streams the file list. The
receiver compares the list with the destination.

**Requesting files.** The receiver sends the sender a block signature of
each file it needs. Signatures are rsync's rolling checksum plus a truncated
SHA-256. Block size is about √size, kept between 700 B and 128 KiB.

**Delta and verification.** The sender answers with a delta of copy and
literal operations. The receiver rebuilds the file in a temp file next to
the target, checks the whole-file SHA-256, and renames it into place.

**Resume.** With `--partial`, an interrupted file is kept as
`.<name>.xt-partial`. On the next run the receiver sends the partial file's
length and hash; if they match the source's prefix, only the remainder is
sent.

## Development

```
go vet ./...
go test ./...          # unit tests + end-to-end tests of the real binary
go test -race ./...
```

The end-to-end tests run the built binary. They reach the "remote" side
through a fake remote shell, a script that drops the host argument and runs
the command locally, so they need no ssh server. To include the end-to-end
runs in coverage:

```
mkdir -p /tmp/cov && XT_E2E_COVER=1 GOCOVERDIR=/tmp/cov go test .
go tool covdata percent -i /tmp/cov
```
