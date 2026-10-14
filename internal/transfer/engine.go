package transfer

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/jlaffaye/ftp"
)

const errorTail = 20

var tempSequence atomic.Uint64

var errResumeRestart = errors.New("remote file changed or server cannot resume; a fresh download is required")

type settings struct {
	address, user, password string
	roots                   []string
	workers, retries        int
	timeout                 time.Duration
	auto                    bool
	tls                     *tls.Config
}

type job struct {
	remote, local string
	size          int64
	mtime         time.Time
}

type state struct {
	mu       sync.Mutex
	snapshot Snapshot
	started  time.Time
	errs     []error
	secret   string
	files    map[string]FileProgress
}

func (s *state) update(fn func(*Snapshot)) { s.mu.Lock(); fn(&s.snapshot); s.mu.Unlock() }
func (s *state) beginFile(j job) {
	s.mu.Lock()
	s.snapshot.Active++
	s.snapshot.Current = j.remote
	if s.files == nil {
		s.files = make(map[string]FileProgress)
	}
	s.files[j.remote] = FileProgress{Path: j.remote, TotalBytes: j.size}
	s.mu.Unlock()
}

func (s *state) recordBytes(remote string, received, written int64) {
	s.mu.Lock()
	file := s.files[remote]
	file.Bytes += written
	s.files[remote] = file
	s.snapshot.Bytes += received
	s.snapshot.ProgressBytes += written
	s.mu.Unlock()
}

func (s *state) resumeFile(remote string, offset int64) {
	s.mu.Lock()
	file := s.files[remote]
	file.Bytes += offset
	file.ResumeOffset = offset
	s.files[remote] = file
	s.snapshot.ProgressBytes += offset
	s.mu.Unlock()
}

func (s *state) refreshFile(j job) {
	s.mu.Lock()
	file := s.files[j.remote]
	s.snapshot.TotalBytes += j.size - file.TotalBytes
	file.TotalBytes = j.size
	s.files[j.remote] = file
	s.mu.Unlock()
}

func (s *state) resetFile(remote string) {
	s.mu.Lock()
	file := s.files[remote]
	s.snapshot.ProgressBytes -= file.Bytes
	file.Bytes = 0
	file.ResumeOffset = 0
	s.files[remote] = file
	s.mu.Unlock()
}

func (s *state) finishFile(remote string, completed bool) {
	s.mu.Lock()
	if completed {
		s.snapshot.Completed++
		if s.files[remote].ResumeOffset > 0 {
			s.snapshot.Resumed++
		}
	}
	delete(s.files, remote)
	s.snapshot.Active--
	s.mu.Unlock()
}

func (s *state) snapshotFiles() []FileProgress {
	files := make([]FileProgress, 0, len(s.files))
	for _, file := range s.files {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}
func (s *state) listing() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.snapshot.Listing }
func (s *state) fail(err error) {
	if err == nil {
		return
	}
	if s.secret != "" {
		err = errors.New(strings.ReplaceAll(err.Error(), s.secret, "[redacted]"))
	}
	s.mu.Lock()
	s.snapshot.Failed++
	if len(s.snapshot.Errors) == errorTail {
		s.snapshot.Errors = append(s.snapshot.Errors[:0], s.snapshot.Errors[1:]...)
	}
	s.snapshot.Errors = append(s.snapshot.Errors, err.Error())
	if len(s.errs) == errorTail {
		s.errs = append(s.errs[:0], s.errs[1:]...)
	}
	s.errs = append(s.errs, err)
	s.mu.Unlock()
}
func (s *state) sample() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.snapshot
	r.Errors = append([]string(nil), s.snapshot.Errors...)
	r.ActiveFiles = s.snapshotFiles()
	r.Elapsed = time.Since(s.started)
	return r
}

// Run downloads selected FTP files through a bounded discovery/worker pipeline.
// Progress callbacks are serial, sampled at ten Hz with a final sample.
// A failed file or directory makes the result an error, even after partial success.
func Run(ctx context.Context, cfg Config, progress func(Snapshot)) (Snapshot, error) {
	set, err := validate(cfg)
	if err != nil {
		return Snapshot{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st := &state{started: time.Now(), secret: set.password, snapshot: Snapshot{Workers: set.workers, Listing: true}}
	if cfg.DryRun {
		st.snapshot.Workers = 0
	}

	var root *os.Root
	if !cfg.DryRun {
		if err := os.MkdirAll(cfg.Destination, 0755); err != nil {
			return st.sample(), fmt.Errorf("create destination: %w", err)
		}
		root, err = os.OpenRoot(cfg.Destination)
		if err != nil {
			return st.sample(), fmt.Errorf("open destination: %w", err)
		}
		defer root.Close()
	}

	var store *resumeStore
	if cfg.Resume && !cfg.DryRun {
		store, err = openResumeStore(root)
		if err != nil {
			return st.sample(), err
		}
		defer store.Close()
	}

	conns := newConnSet()
	defer conns.closeAll()
	go func() { <-ctx.Done(); conns.closeAll() }()
	jobs := make(chan job, set.workers*2)
	var workers sync.WaitGroup
	if !cfg.DryRun {
		for range set.workers {
			workers.Add(1)
			go func() { defer workers.Done(); worker(ctx, set, root, jobs, st, conns, cfg, store) }()
		}
	}
	stopProgress := make(chan struct{})
	var progressWG sync.WaitGroup
	if progress != nil {
		progressWG.Add(1)
		go func() {
			defer progressWG.Done()
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					progress(st.sample())
				case <-stopProgress:
					return
				}
			}
		}()
	}

	discover(ctx, set, cfg, root, jobs, st, conns)
	st.update(func(p *Snapshot) { p.Listing = false })
	close(jobs)
	workers.Wait()
	st.update(func(p *Snapshot) { p.Listing = false; p.Active = 0 })
	close(stopProgress)
	progressWG.Wait()
	final := st.sample()
	if progress != nil {
		progress(final)
	}
	if ctx.Err() != nil {
		return final, ctx.Err()
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.errs) > 0 {
		return final, errors.Join(st.errs...)
	}
	return final, nil
}

func validate(cfg Config) (settings, error) {
	if cfg.Destination == "" {
		return settings{}, errors.New("destination is required")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "ftp" && u.Scheme != "ftps") {
		return settings{}, errors.New("URL must be an ftp:// or ftps:// URL with a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return settings{}, errors.New("URL query and fragment are not supported")
	}
	if cfg.Connections < 0 || cfg.Connections > 128 {
		return settings{}, errors.New("connections must be between 0 (auto) and 128")
	}
	if cfg.Retries < 0 {
		return settings{}, errors.New("retries must not be negative")
	}
	if cfg.MinSize < 0 || cfg.MaxSize < 0 || (cfg.MaxSize > 0 && cfg.MinSize > cfg.MaxSize) {
		return settings{}, errors.New("invalid size limits")
	}
	for _, pattern := range append(append([]string(nil), cfg.Include...), cfg.Exclude...) {
		if _, err := doublestar.Match(pattern, ""); err != nil {
			return settings{}, fmt.Errorf("invalid glob %q: %w", pattern, err)
		}
	}
	roots := append([]string(nil), cfg.Paths...)
	if len(roots) == 0 {
		roots = []string{u.Path}
	}
	for i, p := range roots {
		if strings.ContainsAny(p, "\r\n\x00") {
			return settings{}, errors.New("remote paths cannot contain line breaks or NUL characters")
		}
		roots[i] = cleanRemote(p)
	}
	unique := make(map[string]struct{}, len(roots))
	basenames := make(map[string]struct{}, len(roots))
	for _, p := range roots {
		name := path.Base(p)
		if _, duplicate := basenames[name]; duplicate {
			return settings{}, errors.New("overlapping remote output paths are not supported")
		}
		basenames[name] = struct{}{}
		unique[p] = struct{}{}
	}
	for _, p := range roots {
		for parent := path.Dir(p); parent != p; parent = path.Dir(parent) {
			if _, overlap := unique[parent]; overlap {
				return settings{}, errors.New("overlapping remote output paths are not supported")
			}
			if parent == "/" {
				break
			}
		}
	}
	port := u.Port()
	if port == "" {
		port = "21"
	}
	host := u.Hostname()
	if host == "" {
		return settings{}, errors.New("URL host is required")
	}
	user, pass := "anonymous", "anonymous@"
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	if cfg.Username != "" {
		user = cfg.Username
	}
	if cfg.PasswordSet {
		pass = cfg.Password
	} else if cfg.Password != "" {
		pass = cfg.Password
	}
	workers := cfg.Connections
	auto := workers == 0
	if auto {
		workers = runtime.GOMAXPROCS(0)
		if workers < 4 {
			workers = 4
		}
		if workers > 16 {
			workers = 16
		}
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if timeout < 0 {
		return settings{}, errors.New("timeout must not be negative")
	}
	set := settings{address: net.JoinHostPort(host, port), user: user, password: pass, roots: roots, workers: workers, retries: cfg.Retries, timeout: timeout, auto: auto}
	if u.Scheme == "ftps" {
		set.tls = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.Insecure, ClientSessionCache: tls.NewLRUClientSessionCache(workers + 1)}
	}
	return set, nil
}

func cleanRemote(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean("/" + p)
}

func discover(ctx context.Context, set settings, cfg Config, root *os.Root, jobs chan<- job, st *state, conns *connSet) {
	c, err := dial(ctx, set, conns)
	if err != nil {
		st.fail(fmt.Errorf("connect for discovery: %w", err))
		return
	}
	defer c.Quit()
	for _, remote := range set.roots {
		if ctx.Err() != nil {
			return
		}
		entry, err := rootEntry(c, remote)
		if err != nil {
			st.fail(fmt.Errorf("inspect remote path %q: %w", remote, err))
			continue
		}
		if entry.Type == ftp.EntryTypeLink {
			st.update(func(p *Snapshot) { p.Skipped++ })
			continue
		}
		if entry.Type == ftp.EntryTypeFile {
			name := path.Base(remote)
			if !safeName(name) || entry.Size > uint64(1<<63-1) {
				st.fail(fmt.Errorf("invalid remote file metadata for %q", remote))
				continue
			}
			emit(ctx, cfg, st, jobs, job{remote: remote, local: name, size: int64(entry.Size), mtime: entry.Time})
			continue
		}
		if entry.Type != ftp.EntryTypeFolder {
			st.fail(fmt.Errorf("unsupported remote path %q", remote))
			continue
		}
		entries, err := c.List(remote)
		if err != nil {
			st.fail(fmt.Errorf("list remote directory %q: %w", remote, err))
			continue
		}
		prefix := ""
		if len(set.roots) > 1 {
			prefix = path.Base(remote)
			if protectedDestination(prefix) {
				st.fail(errors.New(".jesusftp-resume is reserved for download-resume state"))
				continue
			}
			if root != nil {
				if err := root.MkdirAll(prefix, 0755); err != nil {
					st.fail(fmt.Errorf("create local directory %q: %w", prefix, err))
					continue
				}
			}
		}
		walk(ctx, c, remote, prefix, entries, cfg, root, st, jobs)
	}
}

func rootEntry(c *ftp.ServerConn, remote string) (*ftp.Entry, error) {
	if entry, err := c.GetEntry(remote); err == nil {
		return entry, nil
	}
	if size, err := c.FileSize(remote); err == nil && size >= 0 {
		mtime, _ := c.GetTime(remote)
		return &ftp.Entry{Name: path.Base(remote), Type: ftp.EntryTypeFile, Size: uint64(size), Time: mtime}, nil
	}
	if err := c.ChangeDir(remote); err == nil {
		return &ftp.Entry{Type: ftp.EntryTypeFolder}, nil
	}
	entries, err := c.List(path.Dir(remote))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name == path.Base(remote) {
			return entry, nil
		}
	}
	return nil, errors.New("remote path not found or inaccessible")
}

func walk(ctx context.Context, c *ftp.ServerConn, dir, prefix string, entries []*ftp.Entry, cfg Config, root *os.Root, st *state, jobs chan<- job) {
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if !safeName(e.Name) {
			st.fail(fmt.Errorf("unsafe remote entry name in %q", dir))
			continue
		}
		remote := path.Join(dir, e.Name)
		local := e.Name
		if prefix != "" {
			local = path.Join(prefix, e.Name)
		}
		if e.Type == ftp.EntryTypeLink {
			continue
		}
		if e.Type == ftp.EntryTypeFolder {
			if matches(cfg.Exclude, local) {
				st.update(func(p *Snapshot) { p.Skipped++ })
				continue
			}
			if protectedDestination(local) {
				st.fail(errors.New(".jesusftp-resume is reserved for download-resume state"))
				continue
			}
			if root != nil {
				if err := root.MkdirAll(local, 0755); err != nil {
					st.fail(fmt.Errorf("create local directory %q: %w", local, err))
					continue
				}
			}
			next, err := c.List(remote)
			if err != nil {
				st.fail(fmt.Errorf("list remote directory %q: %w", remote, err))
				continue
			}
			walk(ctx, c, remote, local, next, cfg, root, st, jobs)
			continue
		}
		if e.Type != ftp.EntryTypeFile {
			st.fail(fmt.Errorf("unknown remote entry type for %q", remote))
			continue
		}
		if e.Size > uint64(1<<63-1) {
			st.fail(fmt.Errorf("invalid remote file metadata for %q", remote))
			continue
		}
		emit(ctx, cfg, st, jobs, job{remote: remote, local: local, size: int64(e.Size), mtime: e.Time})
	}
}

func protectedDestination(local string) bool {
	return local == resumeDir || strings.HasPrefix(local, resumeDir+"/")
}

func emit(ctx context.Context, cfg Config, st *state, jobs chan<- job, j job) {
	if !selected(cfg, j) {
		st.update(func(p *Snapshot) { p.Skipped++ })
		return
	}
	if protectedDestination(j.local) {
		st.fail(errors.New(".jesusftp-resume is reserved for download-resume state"))
		return
	}
	st.update(func(p *Snapshot) { p.Discovered++; p.TotalBytes += j.size })
	if cfg.DryRun {
		return
	}
	select {
	case jobs <- j:
	case <-ctx.Done():
	}
}
func selected(cfg Config, j job) bool {
	if cfg.MinSize > 0 && j.size < cfg.MinSize || cfg.MaxSize > 0 && j.size > cfg.MaxSize || !cfg.After.IsZero() && !j.mtime.After(cfg.After) {
		return false
	}
	if len(cfg.Include) > 0 && !matches(cfg.Include, j.local) {
		return false
	}
	return !matches(cfg.Exclude, j.local)
}
func matches(patterns []string, p string) bool {
	for _, pattern := range patterns {
		ok, _ := doublestar.Match(pattern, p)
		if ok {
			return true
		}
	}
	return false
}
func safeName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00") && filepath.Base(s) == s
}

func worker(ctx context.Context, set settings, root *os.Root, jobs <-chan job, st *state, conns *connSet, cfg Config, store *resumeStore) {
	var source string
	var c *ftp.ServerConn
	var buf []byte
	closeConn := func() {
		if c != nil {
			_ = c.Quit()
			c = nil
		}
	}
	defer closeConn()
	for j := range jobs {
		if ctx.Err() != nil {
			return
		}
		st.beginFile(j)
		if cfg.SkipExisting {
			if info, err := root.Lstat(j.local); err == nil && info.Mode().IsRegular() && info.Size() == j.size {
				st.update(func(p *Snapshot) { p.Skipped++; p.ProgressBytes += j.size })
				st.finishFile(j.remote, false)
				continue
			}
		}
		if store != nil {
			identity := fmt.Sprintf("%t\x00%s\x00%s\x00%s\x00%t", set.tls != nil, set.address, set.user, j.remote, cfg.Insecure)
			source = fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
		}
		if buf == nil {
			buf = make([]byte, 128*1024)
		}
		var err error
		forceFresh := false
		for attempt := 0; ; attempt++ {
			if c == nil {
				c, err = dial(ctx, set, conns)
				if err != nil {
					if set.auto && st.listing() && retry(ctx, attempt, 30) {
						continue
					}
					if !retry(ctx, attempt, set.retries) {
						break
					}
					continue
				}
			}
			err = download(ctx, c, root, j, st, buf, store, source, forceFresh)
			if errors.Is(err, errResumeRestart) && !forceFresh {
				// Reconnect to clear any REST marker left by a rejecting server.
				// This capability fallback does not consume a user retry.
				forceFresh = true
				closeConn()
				c, err = dial(ctx, set, conns)
				if err == nil {
					err = download(ctx, c, root, j, st, buf, store, source, true)
				}
			}
			if err == nil {
				break
			}
			closeConn()
			if !retry(ctx, attempt, set.retries) {
				break
			}
		}
		st.finishFile(j.remote, err == nil)
		if err != nil {
			st.fail(fmt.Errorf("download %q: %w", j.remote, err))
		}
	}
}

func retry(ctx context.Context, attempt, max int) bool {
	if attempt >= max {
		return false
	}
	d := time.Duration(1<<min(attempt, 4)) * 100 * time.Millisecond
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func download(ctx context.Context, c *ftp.ServerConn, root *os.Root, j job, st *state, buf []byte, store *resumeStore, source string, forceFresh bool) error {
	parent := path.Dir(j.local)
	if parent != "." {
		if err := root.MkdirAll(parent, 0755); err != nil {
			return err
		}
	}
	var partial *partialFile
	var f *os.File
	var tmp string
	var offset int64
	if store != nil {
		fresh, err := currentResumeJob(c, j)
		if err != nil {
			return err
		}
		j = fresh
		st.refreshFile(j)
		partial, err = store.prepare(source, j)
		if err != nil {
			return err
		}
		defer partial.Close()
		if forceFresh {
			if err := partial.Reset(); err != nil {
				return err
			}
		}
		f, tmp, offset = partial.File, partial.Path, partial.Offset
	} else {
		tmp = path.Join(parent, fmt.Sprintf(".jesusftp-%d-%d", time.Now().UnixNano(), tempSequence.Add(1)))
		var err error
		f, err = root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close(); _ = root.Remove(tmp) }()
	}
	committed := false
	defer func() {
		if !committed {
			st.resetFile(j.remote)
		}
	}()
	resp, err := c.RetrFrom(j.remote, uint64(offset))
	if err != nil {
		var reply *textproto.Error
		if offset > 0 && errors.As(err, &reply) && reply.Code >= 500 {
			return errResumeRestart
		}
		return err
	}
	if offset > 0 {
		st.resumeFile(j.remote, offset)
	}
	n := offset
	for {
		rn, re := resp.Read(buf)
		if rn > 0 {
			if offset > 0 && int64(rn) > j.size-n {
				// A server may accept REST but ignore the offset. Do not append
				// a second copy or preserve this untrustworthy prefix.
				st.recordBytes(j.remote, int64(rn), 0)
				_ = resp.Close()
				if err := partial.Reset(); err != nil {
					return err
				}
				return errResumeRestart
			}
			wn, we := f.Write(buf[:rn])
			n += int64(wn)
			st.recordBytes(j.remote, int64(rn), int64(wn))
			if we != nil || wn != rn {
				_ = resp.Close()
				if we != nil {
					return we
				}
				return io.ErrShortWrite
			}
		}
		if re == io.EOF {
			break
		}
		if re != nil {
			_ = resp.Close()
			return re
		}
		select {
		case <-ctx.Done():
			_ = resp.Close()
			return ctx.Err()
		default:
		}
	}
	if err := resp.Close(); err != nil {
		return err
	}
	if j.size >= 0 && n != j.size {
		return fmt.Errorf("size mismatch: got %d, expected %d", n, j.size)
	}
	if store != nil {
		fresh, err := currentResumeJob(c, j)
		if err != nil {
			return err
		}
		if fresh.size != j.size || !fresh.mtime.Equal(j.mtime) {
			if err := partial.Reset(); err != nil {
				return err
			}
			return errResumeRestart
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !j.mtime.IsZero() {
		if err := root.Chtimes(tmp, j.mtime, j.mtime); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Rename(tmp, j.local); err != nil {
		return err
	}
	committed = true
	if partial != nil {
		partial.Finish()
	}
	return nil
}

func currentResumeJob(c *ftp.ServerConn, j job) (job, error) {
	entry, err := rootEntry(c, j.remote)
	if err != nil {
		return j, fmt.Errorf("inspect remote file for resume: %w", err)
	}
	if entry.Type != ftp.EntryTypeFile || entry.Size > uint64(1<<63-1) {
		return j, errors.New("cannot resume: remote path is not a regular file or its size exceeds the supported range")
	}
	j.size, j.mtime = int64(entry.Size), entry.Time
	return j, nil
}

type connSet struct {
	mu     sync.Mutex
	conns  map[*deadlineConn]struct{}
	closed bool
}

func newConnSet() *connSet { return &connSet{conns: make(map[*deadlineConn]struct{})} }
func (s *connSet) add(c *deadlineConn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}
func (s *connSet) remove(c *deadlineConn) { s.mu.Lock(); delete(s.conns, c); s.mu.Unlock() }
func (s *connSet) closeAll() {
	s.mu.Lock()
	s.closed = true
	all := make([]*deadlineConn, 0, len(s.conns))
	for c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}

type deadlineConn struct {
	net.Conn
	timeout time.Duration
	set     *connSet
	closed  atomic.Bool
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}
func (c *deadlineConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}
func (c *deadlineConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.set.remove(c)
	}
	return c.Conn.Close()
}

func dial(ctx context.Context, set settings, conns *connSet) (*ftp.ServerConn, error) {
	dialer := net.Dialer{Timeout: set.timeout}
	fn := func(network, address string) (net.Conn, error) {
		n, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		wrapped := &deadlineConn{Conn: n, timeout: set.timeout, set: conns}
		conns.add(wrapped)
		if set.tls != nil && address != set.address {
			return tls.Client(wrapped, set.tls), nil
		}
		return wrapped, nil
	}
	opts := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithDialFunc(fn), ftp.DialWithTimeout(set.timeout), ftp.DialWithShutTimeout(set.timeout)}
	if set.tls != nil {
		opts = append(opts, ftp.DialWithExplicitTLS(set.tls))
	}
	c, err := ftp.Dial(set.address, opts...)
	if err != nil {
		return nil, err
	}
	if err := c.Login(set.user, set.password); err != nil {
		_ = c.Quit()
		message := err.Error()
		if set.password != "" {
			message = strings.ReplaceAll(message, set.password, "[redacted]")
		}
		return nil, fmt.Errorf("FTP login: %s", message)
	}
	return c, nil
}
