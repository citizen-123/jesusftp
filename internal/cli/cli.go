// Package cli provides the jesusftp command-line frontend.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/citizen-123/jesusftp/internal/transfer"
	"golang.org/x/sys/unix"
)

// Version is replaced at build time with -ldflags
// "-X github.com/citizen-123/jesusftp/internal/cli.Version=…".
var Version = "dev"

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type options struct {
	output, from           string
	connections, retries   int
	minSize, maxSize       int64
	after                  time.Time
	timeout                time.Duration
	user                   string
	include, exclude       stringList
	skipExisting, dryRun   bool
	resume                 bool
	insecure, plain, quiet bool
	json, help, version    bool
}

// Run executes the command and returns a process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "config" {
		return runConfig(args[1:], stdout, stderr)
	}
	opts, positional, err := parse(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "jesusftp: %s\nUsage: jesusftp --help\n", safeText(err.Error()))
		return 2
	}
	if opts.help {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	if opts.version {
		fmt.Fprintf(stdout, "jesusftp %s\n", Version)
		return 0
	}
	if len(positional) == 0 {
		fmt.Fprint(stderr, "jesusftp: an FTP URL or connection alias is required\nUsage: jesusftp --help\n")
		return 2
	}
	positional[0], opts.user, err = resolveConnection(positional[0], opts.user)
	if err != nil {
		fmt.Fprintf(stderr, "jesusftp: %s\n", safeText(err.Error()))
		return 2
	}
	if err := validURL(positional[0]); err != nil {
		fmt.Fprintf(stderr, "jesusftp: %s\n", err)
		return 2
	}

	paths, err := appendPaths(positional[1:], opts.from, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "jesusftp: %s\n", safeText(err.Error()))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	password, passwordSet, err := passwordFor(ctx, positional[0], opts.user)
	if err != nil {
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return 130
		}
		fmt.Fprintf(stderr, "jesusftp: cannot read password: %s\n", safeText(err.Error()))
		return 1
	}
	cfg := transfer.Config{
		URL: positional[0], Paths: paths, Destination: opts.output, Username: opts.user,
		Password: password, PasswordSet: passwordSet, Connections: opts.connections, Retries: opts.retries,
		Timeout: opts.timeout, Include: opts.include, Exclude: opts.exclude,
		MinSize: opts.minSize, MaxSize: opts.maxSize, After: opts.after,
		SkipExisting: opts.skipExisting, DryRun: opts.dryRun, Insecure: opts.insecure,
		Resume: opts.resume,
	}

	interactive := isTerminal(stdin) && isTerminal(stderr) && !opts.plain && !opts.quiet && !opts.json && os.Getenv("TERM") != "dumb"
	var snap transfer.Snapshot
	if interactive {
		snap, err = runTUI(ctx, stop, cfg, stdin, stderr, os.Getenv("NO_COLOR") == "")
	} else {
		snap, err = transfer.Run(ctx, cfg, nil)
	}
	if opts.json {
		writeErr := json.NewEncoder(stdout).Encode(struct {
			Snapshot transfer.Snapshot `json:"snapshot"`
			Error    string            `json:"error"`
		}{snap, safeText(errorText(err))})
		if writeErr != nil {
			fmt.Fprintf(stderr, "jesusftp: write JSON: %s\n", safeText(writeErr.Error()))
			return 1
		}
	} else if !opts.quiet {
		printSummary(stderr, snap, err, cfg.DryRun)
	} else if err != nil {
		fmt.Fprintf(stderr, "jesusftp: %s\n", safeText(errorText(err)))
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return 130
	}
	return 1
}

func parse(args []string, stderr io.Writer) (options, []string, error) {
	var o options
	fs := flag.NewFlagSet("jesusftp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.output, "output", ".", "")
	fs.StringVar(&o.output, "o", ".", "")
	fs.IntVar(&o.connections, "connections", 0, "")
	fs.IntVar(&o.connections, "j", 0, "")
	fs.Var(&o.include, "include", "")
	fs.Var(&o.exclude, "exclude", "")
	fs.BoolVar(&o.skipExisting, "skip-existing", false, "")
	fs.BoolVar(&o.resume, "resume", false, "")
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "")
	fs.IntVar(&o.retries, "retries", 2, "")
	fs.StringVar(&o.user, "user", "", "")
	fs.BoolVar(&o.insecure, "insecure", false, "")
	fs.BoolVar(&o.plain, "plain", false, "")
	fs.BoolVar(&o.quiet, "quiet", false, "")
	fs.BoolVar(&o.quiet, "q", false, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.StringVar(&o.from, "from", "", "")
	fs.BoolVar(&o.help, "help", false, "")
	fs.BoolVar(&o.help, "h", false, "")
	fs.BoolVar(&o.version, "version", false, "")

	// flag.Parse intentionally sees only flags, so options may appear on either
	// side of the URL. A literal -- keeps every following token positional.
	knownValue := map[string]bool{"--output": true, "-o": true, "--connections": true, "-j": true, "--include": true, "--exclude": true, "--min-size": true, "--max-size": true, "--after": true, "--timeout": true, "--retries": true, "--user": true, "--from": true}
	var flags, positional []string
	literal := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if literal {
			positional = append(positional, a)
			continue
		}
		if a == "--" {
			literal = true
			continue
		}
		key := a
		if n := strings.IndexByte(key, '='); n >= 0 {
			key = key[:n]
		}
		if _, ok := knownValue[key]; ok {
			flags = append(flags, a)
			if !strings.Contains(a, "=") {
				if i+1 >= len(args) {
					return o, nil, fmt.Errorf("flag needs an argument: %s", key)
				}
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			continue
		}
		positional = append(positional, a)
	}
	var min, max, after string
	fs.StringVar(&min, "min-size", "", "")
	fs.StringVar(&max, "max-size", "", "")
	fs.StringVar(&after, "after", "", "")
	if err := fs.Parse(flags); err != nil {
		return o, nil, err
	}
	var err error
	if min != "" {
		if o.minSize, err = parseSize(min); err != nil {
			return o, nil, fmt.Errorf("--min-size: %w", err)
		}
	}
	if max != "" {
		if o.maxSize, err = parseSize(max); err != nil {
			return o, nil, fmt.Errorf("--max-size: %w", err)
		}
	}
	if o.maxSize > 0 && o.minSize > o.maxSize {
		return o, nil, errors.New("--min-size cannot exceed --max-size")
	}
	if after != "" {
		if o.after, err = parseAfter(after); err != nil {
			return o, nil, fmt.Errorf("--after: %w", err)
		}
	}
	if o.connections < 0 || o.connections > 128 {
		return o, nil, errors.New("connections must be between 0 (auto) and 128")
	}
	if o.retries < 0 {
		return o, nil, errors.New("retries cannot be negative")
	}
	if o.timeout <= 0 {
		return o, nil, errors.New("timeout must be positive")
	}
	return o, positional, nil
}

func validURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "ftp" && u.Scheme != "ftps") || u.Hostname() == "" {
		return errors.New("URL must be ftp:// or ftps:// with a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("FTP destinations cannot contain a query or fragment")
	}
	return nil
}
func passwordFor(ctx context.Context, rawURL, override string) (string, bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false, err
	}
	user := ""
	hasPassword := false
	if u.User != nil {
		user = u.User.Username()
		_, hasPassword = u.User.Password()
	}
	if override != "" {
		user = override
	}
	if hasPassword || user == "" || strings.EqualFold(user, "anonymous") {
		return "", false, nil
	}
	password, err := readPassword(ctx)
	return password, err == nil, err
}

func readPassword(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no controlling terminal: %w", err)
	}
	defer tty.Close()
	fd := int(tty.Fd())
	state, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	hidden := *state
	hidden.Lflag &^= unix.ECHO | unix.ECHONL
	hidden.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &hidden); err != nil {
		return "", err
	}
	// Discard any unfinished password line before returning control to the shell.
	defer unix.IoctlSetTermios(fd, unix.TCSETSF, state)
	if _, err := fmt.Fprint(tty, "Password (hidden, not saved): "); err != nil {
		return "", err
	}
	defer fmt.Fprintln(tty)
	// A blocking read on a terminal is not interrupted by closing its fd from
	// another goroutine. Poll a nonblocking fd instead so signals always restore
	// terminal settings and stop promptly, without a stranded reader goroutine.
	if err := unix.SetNonblock(fd, true); err != nil {
		return "", err
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	var buffer [4096]byte
	var password []byte
	defer func() { clear(buffer[:]); clear(password) }()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ready, err := unix.Poll(poll, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return "", err
		}
		if ready == 0 {
			continue
		}
		n, err := unix.Read(fd, buffer[:])
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.EOF
		}
		for _, b := range buffer[:n] {
			if b == '\n' {
				return string(password), nil
			}
			password = append(password, b)
		}
	}
}

func appendPaths(paths []string, from string, in io.Reader) ([]string, error) {
	if from == "" {
		return paths, nil
	}
	var r io.Reader
	var closeFile *os.File
	if from == "-" {
		r = in
	} else {
		f, err := os.Open(filepath.Clean(from))
		if err != nil {
			return nil, err
		}
		closeFile, r = f, f
	}
	if closeFile != nil {
		defer closeFile.Close()
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		if line := strings.TrimSuffix(scanner.Text(), "\r"); line != "" {
			paths = append(paths, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read path manifest: %w", err)
	}
	if len(paths) == 0 {
		return nil, errors.New("path manifest contains no paths")
	}
	return paths, nil
}
func parseSize(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	units := []struct {
		suffix     string
		multiplier uint64
	}{
		{"TBL", 1 << 40}, {"GBL", 1 << 30}, {"MBL", 1 << 20}, {"KBL", 1 << 10}, {"BLESSINGS", 1}, {"BLESSING", 1}, {"BL", 1},
		{"TIB", 1 << 40}, {"TB", 1000000000000}, {"T", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1000000000}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1000000}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1000}, {"K", 1 << 10}, {"B", 1},
	}
	multiplier := uint64(1)
	for _, unit := range units {
		if strings.HasSuffix(v, unit.suffix) {
			v = strings.TrimSpace(strings.TrimSuffix(v, unit.suffix))
			multiplier = unit.multiplier
			break
		}
	}
	n, err := strconv.ParseUint(v, 10, 63)
	if err != nil || n > uint64(1<<63-1)/multiplier {
		return 0, errors.New("size must be a non-negative whole number of blessings (bl), optionally scaled by Kbl, Mbl, Gbl, or Tbl (powers of 1024); maximum is 9223372036854775807 bl")
	}
	return int64(n * multiplier), nil
}
func parseAfter(v string) (time.Time, error) {
	if t, e := time.Parse(time.RFC3339, v); e == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}
func isTerminal(v interface{}) bool {
	if f, ok := v.(interface{ Fd() uintptr }); ok {
		return term.IsTerminal(f.Fd())
	}
	return false
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

const helpText = `jesusftp — Let there be files

USAGE
  jesusftp [options] ALIAS [remote-path ...]
  jesusftp [options] ftp[s]://host/path [remote-path ...]
  jesusftp config add NAME URL [--user NAME]
  jesusftp config set NAME URL [--user NAME]
  jesusftp config list | show NAME | remove NAME | path
  jesusftp config --help

OPTIONS
  -o, --output DIR        destination directory (default .)
  -j, --connections N     parallel download workers; 0 chooses automatically
      --include GLOB      download matching files (repeatable)
      --exclude GLOB      skip matching files/directories (repeatable)
      --min-size SIZE     minimum file size (for example 500Kbl or 2Gbl)
      --max-size SIZE     maximum file size (0: unlimited)
      --after TIME        files modified after RFC3339 time or YYYY-MM-DD
      --skip-existing     skip same-size regular destination files
      --resume            resume tracked partial downloads (Lazarus mode)
      --dry-run           preview matching files; do not download or write files
      --timeout DURATION  timeout for a stalled operation (default 30s)
      --retries N         retry each failed download N times (default 2)
      --user NAME         login username; overrides saved user
                          missing passwords are prompted privately, never saved
      --insecure          DISABLE FTPS certificate verification (unsafe)
      --plain             no dashboard; print final summary on stderr
  -q, --quiet             no dashboard or summary; still print errors
      --json              final JSON on stdout; no dashboard
      --from FILE         newline-separated remote paths; - reads standard input
      --version           print the installed version
  -h, --help              show this help

SIZE UNITS
bl means blessings, the app's data-size unit: one blessing is one octet.
1024 bl = 1 Kbl; 1024 Kbl = 1 Mbl, then Gbl and Tbl.
Rates use the same units per second. Whole-number size inputs only.
Ordinary decimal and binary size suffixes also work.

CONNECTIONS AND RESUME
FTP sends credentials and data in plaintext. FTPS encrypts both using explicit
TLS (normally port 21). Neither is SFTP.
Connection aliases (the Book of Connections) live at
$XDG_CONFIG_HOME/jesusftp/config.json, or ~/.config/jesusftp/config.json.
The config stores destinations, never passwords. Explicit remote paths replace
a saved URL path; --user overrides a saved user.
--resume keeps private partials under .jesusftp-resume in the destination.
Matching remote size and modification time permit continuation; otherwise the
file starts afresh. Metadata is not a cryptographic guarantee of identity.
Options may appear before or after the URL or alias; -- ends option parsing.
Quote --include/--exclude patterns so the shell does not expand them.

EXAMPLES — ask, and it shall be downloaded
  jesusftp config add ark ftps://archive.example.org/private --user noah
  jesusftp ark -o ./promised-land
  jesusftp ark --resume -o ./promised-land
  jesusftp ark /daily /reports --include '**/*.zip' --min-size 1Mbl
  printf '/daily\n/reports\n' | jesusftp ark --from - --dry-run
`
