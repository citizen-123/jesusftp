# JESUSFTP — The Good Transfer

**Let there be files.** A single-binary FTP/FTPS downloader with parallel workers, per-file progress, and a biblical sense of humor. Status labels describe real actions; wordplay stays in the captions. Install the command into `~/.local/bin` or `/usr/local/bin`; no daemon or runtime installation is required.

## Why this exists

Downloading a large FTP archive should not mean retyping hostnames, putting a
password in an environment file, watching one ambiguous progress bar, or starting
every interrupted file over. JESUSFTP exists to make that workflow straightforward:
an installed command, saved connection aliases, private password prompts, and
visible progress for each active file.

The biblical wordplay is intentional; unclear status messages are not.
**Let there be files** is the personality. Downloaded, skipped, failed, and
resumed are the actual operations.

## What it does

- Downloads individual files, directory trees, or selected paths over FTP and explicit FTPS.
- Transfers multiple files in parallel, with a scrollable per-file terminal dashboard.
- Saves connection aliases without saving passwords.
- Filters files by path, size, and modification time before downloading them.
- Resumes tracked partial downloads with `--resume`, while checking remote metadata.
- Preserves existing destination files until their replacements download successfully.
- Supports dry runs, same-size skipping, plain output, and JSON summaries for scripts.

## Install and run

Build requirements: **Linux, Go 1.27.1 or newer, Git, and GNU Make**.
No environment file or separate FTP client installation is required.

```sh
git clone https://github.com/citizen-123/jesusftp.git
cd jesusftp
make install
export PATH="$HOME/.local/bin:$PATH"
jesusftp --help
```

Add `~/.local/bin` to your shell's startup configuration if it is not already on
`PATH`. The installed command needs no Go toolchain at runtime. FTPS uses the
operating system's trusted CA certificates.

Then save a connection below, or pass an FTP/FTPS URL directly.

## Connection aliases — the Book of Connections

Thou shalt not memorize hostnames. Give each destination a name:

```sh
jesusftp config add ark ftps://archive.example.org/private --user noah
jesusftp ark -o ./promised-land

# Different remote paths or a different login user for this invocation only.
jesusftp ark /daily /reports --user moses
printf '/daily\n/reports\n' | jesusftp ark --from -

jesusftp config list
jesusftp config show ark
jesusftp config set ark ftps://new.example.org/private --user noah
jesusftp config remove ark
jesusftp config path
```

Connections are stored in `$XDG_CONFIG_HOME/jesusftp/config.json`, falling back
to `~/.config/jesusftp/config.json`. `config path` reveals the actual location.
The configuration is ordinary, editable JSON:

```json
{
  "connections": {
    "ark": {
      "url": "ftps://archive.example.org/private",
      "user": "noah"
    }
  }
}
```

Names are case-sensitive: begin with an ASCII letter, followed by letters,
digits, `_`, or `-`. `config` is reserved. `add` refuses an existing name;
`set` replaces an existing connection completely, including its optional user.
Explicit `--user` wins over the saved user; explicit remote paths replace the
saved URL path and are relative to the server root. Direct URLs still work and
do not read the connection config.

**Passwords are confessed, never engraved on tablets.** Password-bearing URLs
and password fields are refused in saved connections. Missing credentials are
prompted privately on each run. Files are written atomically with permissions
`0600` in a `0700` application directory; concurrent commands preserve each
other's entries. Invalid configuration is reported, never silently replaced.

## Data sizes and rates

Blessings (`bl`) are the app's data-size unit, not a file count: one blessing
is one octet. Each larger unit contains 1024 of the previous:
**bl → Kbl → Mbl → Gbl → Tbl**. `Received` measures newly received data;
`Avg` is its average rate per second over the run. Percentages show each file's
transfer progress, including any resumed prefix.
Use these units with `--min-size` and `--max-size`; bare whole numbers mean
blessings. Ordinary decimal and binary size suffixes are also accepted.


## Download examples

```sh
# A file, or all contents of a directory; anonymous login by default.
jesusftp ftp://ftp.example.org/pub/archive.tar.gz
jesusftp ftps://ftp.example.org/pub -o ./downloads

# Filter while discovering: rejected file bodies never cross the wire.
jesusftp ftps://ftp.example.org/pub -o ./downloads \
  --include '**/*.zip' --exclude '**/obsolete' --min-size 1Mbl

# Retrieve only recent files, or inspect the selection without writing anything.
jesusftp ftp://ftp.example.org/pub --after 2026-10-01
jesusftp ftp://ftp.example.org/pub --dry-run --json

# Restart a completed/partially completed tree without fetching same-size files.
jesusftp ftp://ftp.example.org/pub -o ./downloads --skip-existing

# Lazarus, come forth: rerun this command after an interrupted transfer.
jesusftp ark --resume -o ./promised-land

# Multiple explicit paths on the same server.
jesusftp ftp://ftp.example.org /pub/releases /pub/manual.pdf -o ./downloads
printf '/pub/a.zip\n/pub/b.zip\n' | jesusftp ftp://ftp.example.org --from -
```

`ftps://` means **explicit TLS**, normally port 21, with certificate verification and encrypted control/data connections. It is not SFTP or implicit FTPS on port 990. `ftp://` sends credentials and data in plaintext: prefer FTPS when available.

## Authentication and password prompts

For authenticated servers, `jesusftp` prompts for the password only when the URL or
`--user` selects a non-anonymous account. The prompt does not echo the password:

```sh
jesusftp ftps://ftp.example.org/private --user saint -o ./downloads
```

The Linux password prompt reads from `/dev/tty`, never from piped stdin.
This keeps `printf '/a\n/b\n' | jesusftp ... --from - --user saint` intact.
With no controlling terminal, missing credentials produce an error instead of
reading filenames as a password or silently submitting an empty password.
Ctrl+C or SIGTERM cancels the prompt and restores terminal echo.

An explicit password in the URL is also accepted, but it may be exposed in shell
history or process listings. `--user` overrides the URL username. `--insecure`
explicitly disables certificate verification; use it only for a trusted test
environment. System certificate roots are used normally; Go's `SSL_CERT_FILE`/
`SSL_CERT_DIR` are available on supported Unix platforms for a private CA.

## Parallel downloads

- **4–16 transfer workers automatically**, based on available Go CPU parallelism; `-j N` overrides the default, up to 128.
- Persistent worker-owned FTP sessions; a separate session discovers directories while downloads run.
- Bounded job queue of twice the worker count. No complete-tree manifest before the first download.
- Reused 128 Kbl buffers, allocated only when a worker actually needs one.
- `MLST`/`MLSD` where supported, with `SIZE`, `CWD`, and legacy `LIST` fallbacks.
- Path, size, and modification-time filtering before `RETR`; excluded directories are not traversed.
- Progress sampled at 10 Hz, not one UI message per packet.

Parallelism is **across files**. One large file uses one data connection; there is no segmented-download claim. The network, server, disk, and FTP round trips still set the ceiling. Directory listings are buffered by the FTP library one directory at a time; memory is not independent of the largest individual listing or tree depth. FTP requires a fresh data socket per file, so very high file rates can exhaust a misconfigured server's passive-port pool.

### Measured performance

Local Linux x86-64 smoke run, Go 1.27.1, pyftpdlib 2.2.0 over loopback, source/destination on tmpfs, 16 automatically selected workers:

| Workload | Observed transfer time | Integrity check |
| --- | --- | --- |
| 100,000 tiny files in 100 directories | 29.1 seconds; 19.9 Mbl peak client RSS | Every downloaded file matched its source exactly |
| One 256 Mbl file | 0.123 seconds | Matching SHA-256 |

These are local protocol/scale checks, **not WAN or physical-disk benchmarks**. The scale server used a reusable passive-port range after its default ephemeral-port allocation exhausted available ports. All files were actually downloaded, not skipped or merely enumerated. Separate real-server checks exercised FTP, verified FTPS, certificate rejection, legacy LIST, filters, credentials, manifests, connection quotas, partial failure, and signal cleanup. The actual dashboard was exercised in a terminal, including keyboard cancellation and a 52-column layout.

## Destination layout

- A single directory URL puts its **contents** directly in `--output` (default: current directory).
- A file keeps its basename.
- With multiple explicit roots, each directory keeps its basename; files still keep their basenames.
- Explicit positional/manifest paths replace the URL path and are relative to the **server root**, not the URL directory. They are literal filenames; only the URL itself is percent-decoded.
- Overlapping roots or duplicate output basenames are rejected rather than raced or silently merged.
- Empty directories are preserved. Entries reported as symbolic links are not followed. Servers that dereference links in their listings cannot expose that distinction to the client.
- Existing files are replaced by default, but only after a complete successful download. Remote modification times are preserved when supplied. Files are created privately (`0600`); remote permission bits are not copied.
- `.jesusftp-resume` at the destination root is reserved for private resume state, even when resume is disabled.

## File filters

Patterns use [doublestar](https://github.com/bmatcuk/doublestar) syntax against slash-separated **destination-relative paths**. Quote globs so your shell does not expand them.

- `--include '**/*.csv'`: CSV files at any depth. Repeated includes are ORed.
- `--exclude '**/cache'`: prune matching directories and exclude matching files. Repeated excludes are ORed; exclusions win over inclusion.
- `--min-size` / `--max-size`: inclusive file-size limits. `500Kbl` means 512000 blessings; `1Mbl` means 1048576. Whole numbers only; maximum size `0` means unlimited.
- `--after`: strictly newer than an RFC3339 timestamp or a UTC `YYYY-MM-DD`. Files without usable modification metadata cannot pass this filter. Legacy `LIST` timestamps may have lower precision.
- `--skip-existing`: skips an existing **regular file with equal size**. This is not checksum or content verification; use the default overwrite behavior when same-size contents may have changed.
- `--dry-run`: discovers and filters without creating destination directories or fetching file bodies. It still makes FTP control/listing connections.

Filters select files using remote metadata; they do not scan or transform file contents. Included patterns do not prune ancestor directories because those ancestors may contain matching descendants.

## Terminal dashboard and JSON output

The running banner reads `JESUSFTP   LET THERE BE FILES`.

The interactive dashboard appears only when stdin and stderr are terminals. It shows literal states such as **Listing remote files**, **Downloading**, and **Resuming downloads**, followed by contextual wordplay: **Let there be files** for downloads, **Lazarus, come forth** for resumption. Captions do not rotate into unrelated warnings. Each active file has its own progress bar. Use ↑/↓ or `j`/`k` to scroll, and PgUp/PgDn to change pages. `q` or Ctrl+C **cancels** safely. `NO_COLOR` removes styling; `TERM=dumb` selects plain output.

`Downloaded` shows completed files against discovered matching files; `Skipped`
counts skipped entries, including excluded directories. `Failed` counts errors,
including directory failures. During cancellation, **failed or stopped** includes
interrupted transfers; it does not mean every queued file was attempted.
`Workers` shows active download jobs against the worker limit, not total FTP
connections. `Received` and `Avg` describe network traffic, not retained prefixes.

**Complete · It is finished** is reserved for success. Previews explicitly say
**Dry run complete** and **no files downloaded**; interrupted runs say **Cancelled**;
unsuccessful runs say **Finished with errors**. Final counts use **downloaded**,
**skipped**, **failed**, and **resumed** rather than alternate religious names.

| Option | Behavior |
| --- | --- |
| `--plain` | No dashboard; final summary on stderr |
| `--quiet`, `-q` | No dashboard or summary; errors still appear on stderr |
| `--json` | One final JSON object on stdout; no dashboard |
| `--from FILE` | Newline-separated remote paths; `-` reads stdin; empty manifests are rejected |
| `--timeout 30s` | Timeout for a stalled operation, not the entire download run |
| `--retries 2` | Retry failed downloads twice; reconnect and restart, or continue a validated partial with `--resume` |
| `--resume` | Retain and resume tracked partial downloads (Lazarus mode) |
| `--connections N`, `-j N` | Use N parallel download workers; `0` chooses automatically |
| `--help`, `-h` | Show usage, options, and examples |
| `--version` | Binary version |

Download options may appear before or after the URL or alias. Use `--` to end option parsing before paths beginning with `-`. Configuration management begins with `jesusftp config`; `jesusftp config --help` explains its subcommands.

JSON has `snapshot` and `error` fields. Snapshot keys include `Discovered`, `Completed`, `Resumed`, `Skipped`, `Failed`, `Blessings`, `ProgressBlessings`, `TotalBlessings`, `Active`, `Workers`, `Listing`, `Current`, `ActiveFiles`, `Elapsed`, and `Errors`. `ActiveFiles` contains one `{Path, Blessings, TotalBlessings, ResurrectedBlessings}` entry per active transfer. **These JSON keys are unchanged by the clearer interface wording.** `Elapsed` is nanoseconds. `Blessings` counts all received data, including retransmissions, but never credits a retained prefix as new traffic. `ProgressBlessings` includes the active retained prefix and same-size skipped files, and removes failed attempts. `ResurrectedBlessings` is the active file's reused prefix; `Resumed` counts successfully completed files that reused one. Totals grow during discovery and may change when resume mode refreshes remote metadata. `Skipped` also counts pruned directory entries, not the unknown number of descendants beneath them. `Failed` may include directory failures and cancelled transfers. Only the latest 20 errors are retained.

Exit codes: **0** success, **1** transfer/output/credential failure (including partial success), **2** invalid invocation or configuration, **130** cancellation. Theology does not change process semantics.

## File safety

By default, files stream into unique `.jesusftp-*` temporaries beside the destination. Only a successful FTP completion response, exact expected size, closed local file, and timestamp update permit the final rename. Without `--resume`, failed/interrupted attempts are removed. Existing destination files remain intact until a complete transfer succeeds. This validates transfer size and protocol completion, **not cryptographic integrity**. Atomic rename is not a power-loss durability guarantee; no per-file `fsync` is imposed on the fast path.

Destination operations use Go's `os.Root` to confine filesystem access, including symlink traversal. Unsafe listing names are rejected; terminal control characters are removed from displayed remote text. Server-provided error strings are redacted for the active password.

### Resuming downloads — Lazarus, come forth

`--resume` retains interrupted transfers in a private `.jesusftp-resume` directory
under the destination. Rerun with the same source, destination, and `--resume`;
automatic retries can reuse the same tracked partial too. No passwords are saved.

- A partial is reused only when its connection identity, remote path, size, and available modification time match. Remote metadata is refreshed before and after transfer. Missing timestamps or changed metadata cause a fresh download.
- A server that rejects REST, or sends an overlong response after accepting it, gets a fresh connection and a full restart—even with `--retries 0`.
- Existing final files and untracked old temporaries are never treated as resumable prefixes. Even a full-size tracked partial still requires a successful FTP completion before promotion.
- Per-file bars include the retained prefix; the dashboard announces **Resuming downloads · Lazarus, come forth** and the final summary counts **resumed** files. Network rates count only newly received data.
- Completed files are not automatically skipped. `--skip-existing` takes precedence when explicitly requested, but equal size is not proof of equal contents. `--dry-run` creates no partial state.
- One Lazarus invocation owns a destination at a time; its workers still download files in parallel. Successful transfers remove their tracking files. With no run active, the directory may be removed to discard retained partials, including leftovers from skipped files.
- Partials and final files must share a filesystem for atomic promotion. Metadata matching is **not a checksum**: it cannot detect changed content whose size and timestamp remain identical, or establish trust in a dishonest server.

Without Lazarus mode, retries start fresh. SIGKILL, machine failure, or process
termination without cleanup can leave ordinary `.jesusftp-*` temporaries; these
are never promoted or blindly resumed. Local files absent from the server are
**not deleted**.

A discovery connection is separate from transfer connections. Extremely restrictive servers may need fewer workers with `-j 1`, and concurrent discovery/transfers require at least two permitted control sessions. Auto mode backs off on connection pressure; it cannot bypass server quotas. Unreadable directories fail the run rather than silently reporting a complete mirror. The underlying FTP library may ignore listing records it cannot parse; use standards-compliant MLSD/LIST servers.

This is a downloader, not an FTP server, uploader, SFTP client, or bidirectional synchronizer.

## Installing and updating the binary

`make install` builds a static binary for the native Linux architecture and installs it as
`~/.local/bin/jesusftp`. Put that directory on `PATH`, or use
`sudo make install PREFIX=/usr/local` for a system-wide install. Rebuild and
replace the installed binary later with:

```sh
make update
```

`make update` rebuilds from the current checkout; it does not fetch a release or
pull source changes. Use the same `PREFIX` or `BINDIR` override as the original
installation. Builds use private, unique staging directories and atomically
replace the installed executable only after success. Failed builds leave the
installed command intact; concurrent installs do not share temporary files.
Set `VERSION=1.2.3` when building or updating to embed a release version.

## Development

Use the clone above. In addition to the build requirements, `make test` needs
a C compiler (GCC or Clang) and CGO enabled for Go's race detector. Tests use
loopback TCP sockets and Linux pseudo-terminals; allow both in sandboxed environments.

```sh
go mod download             # fetch the module dependencies
make build                  # compile the local jesusftp binary
go run . --help             # run the working tree without installing it
make test                   # all tests, including the race detector
```

Format changed Go files with `gofmt` before committing. Use `make update` when
you want to replace the installed command with your current working tree.
For manual download checks, use `-o ./downloads` to keep received files separate
from source files. Never put real credentials in fixtures, examples, or commits.

### Code layout

- `main.go`: executable entrypoint and exit status.
- `internal/cli/`: option parsing, connection configuration, hidden password
  input, terminal dashboard, and summaries.
- `internal/transfer/`: FTP/FTPS discovery, filtering, parallel workers, safe
  destination writes, progress accounting, and resumable partial storage.
- `internal/cli/*_test.go`: CLI behavior, configuration, terminal layout,
  and password-prompt regressions.
- `internal/transfer/*_test.go`: transfer/state regressions and a real-TCP FTP
  fixture, without an external server dependency.
- `Makefile`: native static builds, atomic install/update, and the test command.

### Local files and verification

`.gitignore` excludes compiled binaries, test/profile/coverage outputs, local
workspace files, editor leftovers, environment files and private keys, the example
download destinations (`downloads/` and `promised-land/`), and `.jesusftp-*`
temporary/resume state. Source, `go.mod`, and `go.sum` remain versioned.
Connection configuration normally lives outside the checkout. Downloads to other
destinations are not automatically ignored; keep them outside the repository or
add an explicit local exclusion before staging changes.

The test fixture is implemented in Go and requires no external FTP daemon.
Its cases cover filtering before RETR, truncated transfers, retry
integrity/progress, cancellation of stalled data sockets, destination
confinement, malicious listing paths and overflowing file sizes, literal/long
filenames, symlink-safe skipping, and servers without SIZE. Lazarus regressions
cover cross-run/retry continuation, REST refusal and ignored offsets, changed or
missing metadata, failed FTP completion, retained cancellation, private-state
locking, and symlink/hard-link rejection. CLI regressions cover
connection persistence, username precedence, rejected-secret protection,
concurrent configuration edits, blessing-size boundaries, terminal paging/resizing,
and real pseudo-terminal password cancellation/EOF with terminal restoration.
