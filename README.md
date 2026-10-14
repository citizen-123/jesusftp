# JESUSFTP

**LET THERE BE FILES.**

Download files and folders from an FTP or explicit FTPS server. Download several
files at once, with a progress bar for each one. Save server details under a
nickname, enter passwords privately, and keep interrupted downloads for later.

## Why this exists

Retyping hostnames is annoying. One mystery progress bar tells you very little.
Restarting a large download should not require another act of faith. JESUSFTP
puts connection aliases, parallel downloads, filters, and optional resume in one
installed command. No password environment file required.

This is a **downloader**. It does not provide a server or account, upload files,
synchronize deletions, or speak SFTP.

## Install

You need **Linux, [Go 1.27.1 or newer](https://go.dev/doc/install), Git, and GNU Make**.
This builds from source; the installed binary does not need Go to run.

```sh
git clone https://github.com/citizen-123/jesusftp.git
cd jesusftp
make install
export PATH="$HOME/.local/bin:$PATH"
jesusftp --help
```

The command is installed at `~/.local/bin/jesusftp`. Add that directory to your
shell's startup `PATH` if needed. For a system-wide install, use
`sudo make install PREFIX=/usr/local` instead.

## Download something

**Bring your own server.** Its administrator or provider supplies the address,
remote file/folder path, and login details. The values below are examples, not a
working account: replace `ftp.example.org`, `/private`, and `noah` with yours.

```sh
jesusftp ftps://ftp.example.org/private --user noah -o ./downloads --resume
```

- Enter your password at the hidden prompt. It is not saved. Omit `--user noah`
  only if the server permits anonymous access; the app does not ask for a username.
- `-o ./downloads` puts files in a `downloads` folder under your current directory.
  Downloading `/private` puts its **contents** there, without an extra `private`
  folder. A single `/private/manual.pdf` becomes `./downloads/manual.pdf`.
- Existing files are replaced **only after** their replacement finishes successfully.
- `--resume` keeps tracked partials if interrupted. Use it from the first attempt.

Prefer `ftps://`: it encrypts the connection and checks the server certificate.
It means **explicit FTPS**, normally on port 21—not SFTP or implicit FTPS on port
990. Use `ftp://` only when necessary; it sends credentials and data in plaintext.

The terminal shows each active file's progress. Scroll with ↑/↓ or `j`/`k`, page
with PgUp/PgDn, and cancel with `q` or Ctrl+C. **Let there be files** is the caption;
**Downloaded**, **Skipped**, and **Failed** are the actual counts.

## Stop now, continue later

**Lazarus, come forth.** Rerun the same download command with `--resume`, the same
source, and the same output directory. This only reuses partials saved by an
earlier resume-enabled attempt; adding the flag afterward cannot recover an
ordinary interrupted download.

Resume and skipping are different:

| Option | What it does |
| --- | --- |
| Neither | Downloads selected files again; interrupted partials are normally removed. |
| `--resume` | Keeps tracked partials and continues them when remote metadata still matches. Completed files are downloaded again. |
| `--skip-existing` | Skips an existing regular file with the same size. It does **not** compare contents. |

To resume partials **and** skip equal-size completed files:

```sh
jesusftp ftps://ftp.example.org/private --user noah -o ./downloads --resume --skip-existing
```

`--skip-existing` wins when both apply, even if equal-size contents have changed.
Leave it off when you need to replace potentially stale files. Resume starts fresh
if required timestamps are missing, metadata changes, or the server cannot resume.
Metadata matching is not checksum verification.

## Save a connection — the Book of Connections

Thou shalt not type the same hostname forty times. Save the server address,
remote path, and username under the nickname `ark`:

```sh
jesusftp config add ark ftps://ftp.example.org/private --user noah
jesusftp ark -o ./downloads --resume
```

The second command does the same download as the direct URL above. Passwords are
still prompted, never stored in the alias. Use `jesusftp config list` to see saved
connections and `jesusftp config --help` for optional editing/removal commands.

## Choose what to download

The examples below use `ark` from the previous section. First, count matching ZIP
files and their total size without downloading; then run the same selection:

```sh
jesusftp ark -o ./downloads --resume --include '**/*.zip' --min-size 1Mbl --dry-run
jesusftp ark -o ./downloads --resume --include '**/*.zip' --min-size 1Mbl
```

**A dry run reports totals, not a filename list.** It still connects to the server,
but writes no files or directories. It does not check local files for
`--skip-existing`, so it is not a prediction of how many files that option will skip.

Sizes use **blessings**: `bl`, `Kbl`, `Mbl`, `Gbl`, `Tbl`. One blessing is one octet;
each larger unit is 1024 times the previous one. These measure data, not files.
`1Mbl` means 1048576 blessings; rates use the same units per second.

| Option | Use it for |
| --- | --- |
| `--include '**/*.zip'` | Include matching destination-relative paths. Repeated includes are ORed. |
| `--exclude '**/cache'` | Skip matching files/directories; excluded directories are not traversed. Exclusions win. |
| `--min-size 1Mbl`, `--max-size 2Gbl` | Inclusive size limits; whole numbers only. Maximum `0` means unlimited. Ordinary size suffixes also work. |
| `--after 2026-01-01` | Files strictly newer than a UTC date or RFC3339 timestamp. Missing timestamps do not match. |
| `-j 4` | Four parallel file-download workers. Default: 4–16 automatically; maximum: 128. One large file still uses one connection. |
| `--timeout 30s` | Limit a stalled operation, not the entire download. |
| `--retries 2` | Retry failed downloads twice; reuse tracked partials when `--resume` permits it. |
| `--plain` | No dashboard; final summary on stderr. |
| `--quiet` | No dashboard or summary; errors still appear. |
| `--json` | Machine-readable transfer result on stdout; see the scripting reference below. |

Quote patterns so your shell does not expand them. Use `jesusftp --help` for all
options. Filters use [doublestar syntax](https://github.com/bmatcuk/doublestar).

## Development and updates

From the cloned repository:

```sh
go mod download             # fetch dependencies
make build                  # compile a local jesusftp binary
go run . --help             # run the working tree without installing
make test                   # run all tests with the race detector
```

`make test` also requires **GCC or Clang and CGO enabled**. Tests use loopback TCP
and Linux pseudo-terminals; no separate FTP daemon is needed. Format changed Go
files with `gofmt` before committing.

- `main.go`: executable entrypoint.
- `internal/cli/`: options, saved connections, password prompts, dashboard, summaries.
- `internal/transfer/`: discovery, filters, workers, safe writes, progress, resume.
- Tests live beside the code; transfer tests include a real-TCP FTP fixture.
- `Makefile`: build, atomic install/update, and test commands.

Use `-o ./downloads` for manual checks. That folder, the compiled binary, and
`.jesusftp-*` transfer state are ignored by Git. Downloads elsewhere are not
automatically ignored. Keep credentials out of the checkout and test fixtures.

To update an installed copy from newer source:

```sh
git pull --ff-only
make update
```

`make update` **only rebuilds the current checkout**; it does not fetch updates.
Use the same `PREFIX`/`BINDIR` override as your original install. Failed builds
leave the installed binary intact. `make build VERSION=1.2.3` embeds a version.

## Reference

<details>
<summary>Connections, passwords, and remote paths</summary>

- Aliases are stored in `$XDG_CONFIG_HOME/jesusftp/config.json`, or
  `~/.config/jesusftp/config.json`. `jesusftp config path` prints the location.
  The config is ordinary JSON; `config show ark` prints an alias's saved values.
- `config add` refuses duplicates. `config set NAME URL [--user USER]` replaces
  an existing alias completely, including its optional username.
  `config remove NAME` deletes one. These are maintenance operations, not setup steps.
- Alias names are case-sensitive: an ASCII letter followed by letters, numbers,
  `_`, or `-`. `config` is reserved. Explicit `--user` overrides the saved user.
- The config is saved atomically with private permissions. Password-bearing URLs
  and password fields are rejected in saved connections. Invalid config is not overwritten.
- Password prompts use `/dev/tty`, leaving piped stdin alone. Without a controlling
  terminal, missing required passwords produce an error. URL passwords are accepted
  for direct connections but risk exposure in shell history and process listings.
- Without `-o`, files go into the current directory. A single directory contributes
  its contents; with multiple explicit roots, each directory retains its basename.
  Overlapping roots and duplicate output basenames are rejected.
- Explicit remote paths replace the URL/alias path and are relative to the **server
  root**. For example, `jesusftp ark /private/daily -o ./downloads --resume` selects
  that directory, not `/private/private/daily`. Paths are literal filenames; only
  the URL itself is percent-decoded.
- `--from FILE` reads newline-separated remote paths; `--from -` reads stdin.
  An empty manifest is rejected. Options may come before or after the URL/alias;
  `--` ends option parsing.
- FTPS uses system CA roots. `SSL_CERT_FILE`/`SSL_CERT_DIR` can select a private CA
  on supported Unix platforms. `--insecure` disables certificate verification;
  do not use it as a routine connection fix.

</details>

<details>
<summary>File safety and resume limits</summary>

- Files are promoted only after a successful FTP completion response, exact expected
  size, successful local close, and timestamp update when supplied. Existing final
  files remain intact on failure. This is not cryptographic integrity verification
  or a power-loss durability guarantee; no per-file `fsync` is imposed.
- Files are created privately (`0600`). Remote modification times are preserved
  when available, but remote permission bits are not copied. Empty directories
  are preserved; entries reported as symbolic links are not followed.
- Filesystem access is confined to the destination using Go's `os.Root`.
  Unsafe listing names are rejected. Local files absent from the server are not deleted.
- `--resume` uses the reserved `.jesusftp-resume` directory inside the destination.
  Only one resume-enabled invocation can own that destination at a time; its
  workers still run in parallel. Partials and final files must share a filesystem.
- Reuse checks the connection identity, remote path, size, and available modification
  time. Metadata is refreshed before and after transfer. It cannot detect changed
  content with identical size/timestamp or make a dishonest server trustworthy.
- Unsupported resumption, or an overlong response after a resume request, triggers
  a fresh connection and full restart even with `--retries 0`. Untracked files are
  never treated as partials; even full-size tracked partials require FTP completion.
- Successful transfers remove their tracking files. With no run active, you can
  remove `.jesusftp-resume` to discard retained partials. Abrupt termination can also
  leave ordinary `.jesusftp-*` temporaries; these are not blindly resumed.
- Discovery has its own FTP connection. If the server limits connections, try
  `-j 1`; concurrent discovery and download still need at least two allowed sessions.
  Listings depend on server metadata; legacy formats can lose timestamp precision
  or contain records the FTP library cannot parse. No universal speed claim here.

</details>

<details>
<summary>Dashboard counters, JSON, and exit codes</summary>

The dashboard requires terminal stdin and stderr. `TERM=dumb` selects plain output;
`NO_COLOR` removes styling. `Workers` counts active download jobs against the worker
limit, not all FTP connections. `Received` and `Avg` count new network traffic,
including retries, not retained prefixes; `Avg` is the average over the run.
Per-file percentages include retained prefixes.

After reaching the transfer stage, `--json` emits an object with `snapshot` and
`error` fields. Early CLI, configuration, manifest, or credential errors may instead
leave stdout empty and report on stderr. Always check the exit status. JSON dry
runs report counts and size, not filenames or human captions.

Snapshot keys: `Discovered`, `Completed`, `Resumed`, `Skipped`, `Failed`, `Blessings`,
`ProgressBlessings`, `TotalBlessings`, `Active`, `Workers`, `Listing`, `Current`,
`ActiveFiles`, `Elapsed`, `Errors`.

- `Blessings` counts all received data, including retries.
- `ProgressBlessings` includes completed files, active progress, retained prefixes,
  and same-size skips; failed attempts are removed. Totals can change during discovery
  or resume metadata refreshes.
- `Resumed` counts completed files that reused a prefix. `ActiveFiles` entries have
  `Path`, `Blessings`, `TotalBlessings`, and `ResurrectedBlessings` (the reused prefix).
- `Skipped` counts entries, including excluded directories, not their unknown descendants.
  `Failed` may include directory failures and cancelled transfers. In human cancellation
  summaries, “failed or stopped” does not mean every queued file was attempted.
- `Elapsed` is nanoseconds. Only the latest 20 errors are retained.

Exit codes: **0** success; **1** transfer-stage/validation, output, or credential
failure, including partial success; **2** CLI parsing/configuration/input errors;
**130** cancellation.

</details>
