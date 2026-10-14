package transfer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// wireFTP implements only the protocol used by these tests. It uses real TCP
// data/control sockets so truncation and cancellation exercise the actual client.
type wireFTP struct {
	files       map[string]string
	mode        string
	modifies    map[string]string
	mu          sync.Mutex
	retrieved   []string
	offsets     []int64
	connections map[net.Conn]bool
	started     chan struct{}
	once        sync.Once
	wg          sync.WaitGroup
	listener    net.Listener
}

func newWireFTP(t *testing.T, files map[string]string, mode string) (*wireFTP, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &wireFTP{files: files, mode: mode, modifies: make(map[string]string), connections: make(map[net.Conn]bool), started: make(chan struct{}), listener: l}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.connections[c] = true
			s.mu.Unlock()
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(c) }()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		<-acceptDone
		s.mu.Lock()
		for c := range s.connections {
			c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s, "ftp://" + l.Addr().String()
}

func (s *wireFTP) isDir(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isDirLocked(p)
}

func (s *wireFTP) isDirLocked(p string) bool {
	if p == "/" {
		return true
	}
	for name := range s.files {
		if strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

func (s *wireFTP) facts(p, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.files[p]; ok {
		if s.mode == "oversize" {
			return fmt.Sprintf("type=file;size=18446744073709551615;modify=20260102030405; %s", name)
		}
		facts := fmt.Sprintf("type=file;size=%d;", len(data))
		if modified := s.modifies[p]; modified != "" {
			facts += "modify=" + modified + ";"
		} else if s.mode != "nomtime" {
			facts += "modify=20260102030405;"
		}
		return facts + " " + name
	}
	return "type=dir;modify=20260102030405; " + name
}

func (s *wireFTP) setFile(name, data, modified string) {
	s.mu.Lock()
	s.files[name] = data
	s.modifies[name] = modified
	s.mu.Unlock()
}

func (s *wireFTP) setMode(mode string) {
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
}

func (s *wireFTP) retrievals() ([]string, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.retrieved...), append([]int64(nil), s.offsets...)
}

func (s *wireFTP) serve(c net.Conn) {
	defer func() { c.Close(); s.mu.Lock(); delete(s.connections, c); s.mu.Unlock() }()
	var passive net.Listener
	var offset int64
	defer func() {
		if passive != nil {
			passive.Close()
		}
	}()
	say := func(format string, args ...any) { fmt.Fprintf(c, format+"\r\n", args...) }
	say("220 Test sanctuary")
	scan := bufio.NewScanner(c)
	for scan.Scan() {
		cmd, arg, _ := strings.Cut(scan.Text(), " ")
		p := path.Clean("/" + arg)
		s.mu.Lock()
		mode := s.mode
		_, exists := s.files[p]
		s.mu.Unlock()
		switch cmd {
		case "USER":
			say("331 Password required")
		case "PASS":
			say("230 Welcome")
		case "FEAT":
			say("211-Features\r\n MLST type*;size*;modify*;\r\n UTF8\r\n211 End")
		case "TYPE", "OPTS", "NOOP":
			say("200 OK")
		case "PWD":
			say("257 \"/\"")
		case "CWD":
			if s.isDir(p) {
				say("250 OK")
			} else {
				say("550 Not a directory")
			}
		case "MLST":
			if mode == "nosize" {
				say("502 Unsupported")
				continue
			}
			if !exists && !s.isDir(p) {
				say("550 Not found")
				continue
			}
			say("250-Entry\r\n %s\r\n250 End", s.facts(p, p))
		case "SIZE":
			if mode == "nosize" {
				say("502 Unsupported")
				continue
			}
			s.mu.Lock()
			data, ok := s.files[p]
			s.mu.Unlock()
			if ok {
				say("213 %d", len(data))
			} else {
				say("550 Not a file")
			}
		case "MDTM":
			if mode == "nomtime" {
				say("502 Unsupported")
				continue
			}
			s.mu.Lock()
			modified := s.modifies[p]
			s.mu.Unlock()
			if modified == "" {
				modified = "20260102030405"
			}
			say("213 %s", modified)
		case "REST":
			if mode == "rest-refuse" {
				say("502 REST unsupported")
				continue
			}
			n, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || n < 0 {
				say("501 Invalid offset")
				continue
			}
			if mode == "rest-ignore" {
				offset = 0
			} else {
				offset = n
			}
			say("350 Restarting at %d", n)
		case "EPSV", "PASV":
			if passive != nil {
				passive.Close()
			}
			var err error
			passive, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				say("425 unavailable")
				continue
			}
			port := passive.Addr().(*net.TCPAddr).Port
			if cmd == "EPSV" {
				say("229 Entering Extended Passive Mode (|||%d|)", port)
			} else {
				say("227 Entering Passive Mode (127,0,0,1,%d,%d)", port/256, port%256)
			}
		case "MLSD", "RETR":
			if passive == nil {
				say("425 Use passive mode")
				continue
			}
			say("150 Opening data connection")
			passive.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
			d, err := passive.Accept()
			passive.Close()
			passive = nil
			if err != nil {
				say("425 No data connection")
				continue
			}
			if cmd == "MLSD" {
				s.mu.Lock()
				names := make([]string, 0, len(s.files))
				for name := range s.files {
					names = append(names, name)
				}
				s.mu.Unlock()
				entries := make(map[string]string)
				for _, name := range names {
					rel := strings.TrimPrefix(name, strings.TrimSuffix(p, "/")+"/")
					if rel == name {
						continue
					}
					base, _, _ := strings.Cut(rel, "/")
					entries[base] = s.facts(path.Join(p, base), base)
				}
				names = names[:0]
				for name := range entries {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					fmt.Fprintf(d, "%s\r\n", entries[name])
				}
				if mode == "malicious" {
					fmt.Fprint(d, "type=file;size=4;modify=20260102030405; ../../escaped.txt\r\n")
				}
			} else {
				s.mu.Lock()
				s.retrieved = append(s.retrieved, p)
				s.offsets = append(s.offsets, offset)
				attempt := len(s.retrieved)
				data := s.files[p]
				mode = s.mode
				s.mu.Unlock()
				if offset <= int64(len(data)) {
					data = data[offset:]
				} else {
					data = ""
				}
				if mode == "short" || ((mode == "retry" || mode == "interrupt") && attempt == 1) {
					data = data[:len(data)/2]
				}
				io.WriteString(d, data)
				s.once.Do(func() { close(s.started) })
				if mode == "hang" {
					io.Copy(io.Discard, c)
					d.Close()
					return
				}
			}
			d.Close()
			if mode == "ftp-fail" && cmd == "RETR" {
				say("451 Final transfer failure")
			} else {
				say("226 Complete")
			}
			offset = 0
		case "QUIT":
			say("221 Goodbye")
			return
		default:
			say("502 Command unsupported")
		}
	}
}

func TestWireFiltersBeforeDownload(t *testing.T) {
	s, url := newWireFTP(t, map[string]string{"/tree/a.txt": "keep this", "/tree/tiny.txt": "x", "/tree/nested/b.txt": "nested", "/tree/skip/c.txt": "do not fetch", "/tree/image.bin": "binary"}, "")
	dest := t.TempDir()
	result, err := Run(context.Background(), Config{URL: url + "/tree", Destination: dest, Connections: 3, Include: []string{"**/*.txt"}, Exclude: []string{"skip"}, MinSize: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed != 2 || result.Failed != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	for name, want := range map[string]string{"a.txt": "keep this", "nested/b.txt": "nested"} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
	}
	s.mu.Lock()
	requests := append([]string(nil), s.retrieved...)
	s.mu.Unlock()
	sort.Strings(requests)
	if strings.Join(requests, ",") != "/tree/a.txt,/tree/nested/b.txt" {
		t.Fatalf("filtered data crossed the wire: %v", requests)
	}
}

func TestWireTruncationPreservesExistingFile(t *testing.T) {
	_, url := newWireFTP(t, map[string]string{"/file.txt": "new contents"}, "short")
	dest := t.TempDir()
	final := filepath.Join(dest, "file.txt")
	if err := os.WriteFile(final, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1}, nil)
	if err == nil || result.Failed != 1 {
		t.Fatalf("truncated RETR accepted: %+v, %v", result, err)
	}
	if result.ProgressBytes != 0 {
		t.Fatalf("failed attempt still counted as progress: %d", result.ProgressBytes)
	}
	got, err := os.ReadFile(final)
	if err != nil || string(got) != "original" {
		t.Fatalf("existing destination changed: %q %v", got, err)
	}
	entries, err := os.ReadDir(dest)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v, %v", entries, err)
	}
}

func TestWireRetryStartsFresh(t *testing.T) {
	_, url := newWireFTP(t, map[string]string{"/file.txt": "complete contents"}, "retry")
	dest := t.TempDir()
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Retries: 1}, nil)
	if err != nil || result.Completed != 1 {
		t.Fatalf("retry failed: %+v, %v", result, err)
	}
	if result.ProgressBytes != int64(len("complete contents")) {
		t.Fatalf("retry overcounted progress: %d", result.ProgressBytes)
	}
	got, err := os.ReadFile(filepath.Join(dest, "file.txt"))
	if err != nil || string(got) != "complete contents" {
		t.Fatalf("retry corrupted output: %q %v", got, err)
	}
}

func TestWireCancellationInterruptsStalledData(t *testing.T) {
	s, url := newWireFTP(t, map[string]string{"/file.txt": "some data"}, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dest := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Timeout: time.Minute}, nil)
		done <- err
	}()
	select {
	case <-s.started:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer never started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt data socket")
	}
	entries, err := os.ReadDir(dest)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial destination leaked: %v %v", entries, err)
	}
}

func TestWireDestinationCannotEscapeThroughSymlink(t *testing.T) {
	_, url := newWireFTP(t, map[string]string{"/tree/escape/file.txt": "must stay inside"}, "")
	dest, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "escape")); err != nil {
		t.Skip(err)
	}
	_, err := Run(context.Background(), Config{URL: url + "/tree", Destination: dest, Connections: 1}, nil)
	if err == nil {
		t.Fatal("destination symlink escape accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside destination was modified: %v %v", entries, err)
	}
}

func TestWireRejectsTraversalInListing(t *testing.T) {
	_, url := newWireFTP(t, map[string]string{"/tree/good.txt": "good"}, "malicious")
	parent := t.TempDir()
	dest := filepath.Join(parent, "output")
	_, err := Run(context.Background(), Config{URL: url + "/tree", Destination: dest, Connections: 1}, nil)
	if err == nil {
		t.Fatal("malicious listing name accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal created an outside file: %v", err)
	}
}

func TestWireLiteralManifestAndLongFilename(t *testing.T) {
	for _, name := range []string{"100% holy.txt", strings.Repeat("a", 255)} {
		t.Run(name[:min(10, len(name))], func(t *testing.T) {
			_, url := newWireFTP(t, map[string]string{"/" + name: "saved"}, "")
			dest := t.TempDir()
			_, err := Run(context.Background(), Config{URL: url, Paths: []string{"/" + name}, Destination: dest, Connections: 1}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dest, name))
			if err != nil || string(got) != "saved" {
				t.Fatalf("filename not preserved: %q %v", got, err)
			}
		})
	}
}

func TestWireSkipExistingDoesNotFollowSymlink(t *testing.T) {
	_, url := newWireFTP(t, map[string]string{"/file.txt": "new!"}, "")
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "other"), []byte("old!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", filepath.Join(dest, "file.txt")); err != nil {
		t.Skip(err)
	}
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, SkipExisting: true}, nil)
	if err != nil || result.Completed != 1 {
		t.Fatalf("symlink was treated as existing regular file: %+v %v", result, err)
	}
	info, err := os.Lstat(filepath.Join(dest, "file.txt"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("destination is not a regular file: %v %v", info, err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "other"))
	if err != nil || string(got) != "old!" {
		t.Fatalf("symlink target changed: %q %v", got, err)
	}
}

func TestWireFileWithoutSizeCommand(t *testing.T) {
	_, url := newWireFTP(t, map[string]string{"/file.txt": "legacy contents"}, "nosize")
	dest := t.TempDir()
	_, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "file.txt"))
	if err != nil || string(got) != "legacy contents" {
		t.Fatalf("legacy file misidentified: %q %v", got, err)
	}
}

func TestWireRejectsUnrepresentableListedSize(t *testing.T) {
	s, url := newWireFTP(t, map[string]string{"/tree/file.txt": "incomplete"}, "oversize")
	dest := t.TempDir()
	target := filepath.Join(dest, "file.txt")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Config{URL: url + "/tree", Destination: dest, Connections: 1}, nil)
	if err == nil || result.Failed != 1 || result.Completed != 0 || result.TotalBytes != 0 {
		t.Fatalf("invalid listing size accepted: %+v, %v", result, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "original" {
		t.Fatalf("invalid size replaced existing file: %q %v", got, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.retrieved) != 0 {
		t.Fatalf("invalid file retrieved: %v", s.retrieved)
	}
}

func TestWireResumeAcrossRuns(t *testing.T) {
	const contents = "complete contents for resuming"
	s, url := newWireFTP(t, map[string]string{"/file.txt": contents}, "interrupt")
	dest := t.TempDir()
	final := filepath.Join(dest, "file.txt")
	if err := os.WriteFile(final, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
	if err == nil || first.Failed != 1 || first.ProgressBytes != 0 {
		t.Fatalf("interrupted transfer accepted: %+v, %v", first, err)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != "original" {
		t.Fatalf("partial transfer changed final file: %q, %v", got, err)
	}
	second, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
	if err != nil || second.Completed != 1 || second.Resumed != 1 || second.Bytes != int64(len(contents)-len(contents)/2) || second.ProgressBytes != int64(len(contents)) {
		t.Fatalf("resume result incorrect: %+v, %v", second, err)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != contents {
		t.Fatalf("resumed output incorrect: %q, %v", got, err)
	}
	_, offsets := s.retrievals()
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != int64(len(contents)/2) {
		t.Fatalf("unexpected RETR offsets: %v", offsets)
	}
}

func TestWireResumeRetryUsesPartial(t *testing.T) {
	const contents = "complete contents for retrying"
	s, url := newWireFTP(t, map[string]string{"/file.txt": contents}, "interrupt")
	dest := t.TempDir()
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Retries: 1, Resume: true}, nil)
	if err != nil || result.Completed != 1 || result.Resumed != 1 || result.Bytes != int64(len(contents)) || result.ProgressBytes != int64(len(contents)) {
		t.Fatalf("same-run resume retry incorrect: %+v, %v", result, err)
	}
	_, offsets := s.retrievals()
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != int64(len(contents)/2) {
		t.Fatalf("retry did not resume: %v", offsets)
	}
}

func TestWireResumeRestRefusalRestarts(t *testing.T) {
	const contents = "complete contents after REST refusal"
	s, url := newWireFTP(t, map[string]string{"/file.txt": contents}, "interrupt")
	dest := t.TempDir()
	if _, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil); err == nil {
		t.Fatal("interrupted transfer accepted")
	}
	s.setMode("rest-refuse")
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
	if err != nil || result.Completed != 1 || result.Bytes != int64(len(contents)) || result.ProgressBytes != int64(len(contents)) {
		t.Fatalf("REST refusal did not restart: %+v, %v", result, err)
	}
	_, offsets := s.retrievals()
	if len(offsets) != 2 || offsets[1] != 0 {
		t.Fatalf("REST refusal did not use RETR 0: %v", offsets)
	}
}

func TestWireResumeChangedSourceRestarts(t *testing.T) {
	const original = "first source contents"
	for name, changed := range map[string]string{
		"modification time": "second source content",
		"size":              "changed source has a different length",
	} {
		t.Run(name, func(t *testing.T) {
			s, url := newWireFTP(t, map[string]string{"/file.txt": original}, "interrupt")
			dest := t.TempDir()
			if _, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil); err == nil {
				t.Fatal("interrupted transfer accepted")
			}
			s.setFile("/file.txt", changed, "20260102030406")
			result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
			if err != nil || result.Completed != 1 || result.Bytes != int64(len(changed)) {
				t.Fatalf("changed source did not restart: %+v, %v", result, err)
			}
			_, offsets := s.retrievals()
			if len(offsets) != 2 || offsets[1] != 0 {
				t.Fatalf("changed source reused a prefix: %v", offsets)
			}
		})
	}
}

func TestWireResumeMissingModificationTimeRestarts(t *testing.T) {
	const contents = "contents whose timestamp becomes unavailable"
	s, url := newWireFTP(t, map[string]string{"/file.txt": contents}, "interrupt")
	dest := t.TempDir()
	if _, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil); err == nil {
		t.Fatal("interrupted transfer accepted")
	}
	s.setMode("nomtime")
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
	if err != nil || result.Completed != 1 || result.Bytes != int64(len(contents)) {
		t.Fatalf("missing timestamp did not restart: %+v, %v", result, err)
	}
	_, offsets := s.retrievals()
	if len(offsets) != 2 || offsets[1] != 0 {
		t.Fatalf("missing timestamp reused a prefix: %v", offsets)
	}
}

func TestWireResumeIgnoredRestDoesNotPromoteCorruption(t *testing.T) {
	const contents = "contents that expose ignored REST"
	s, url := newWireFTP(t, map[string]string{"/file.txt": contents}, "interrupt")
	dest := t.TempDir()
	if _, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil); err == nil {
		t.Fatal("interrupted transfer accepted")
	}
	s.setMode("rest-ignore")
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
	if err != nil || result.Completed != 1 || result.ProgressBytes != int64(len(contents)) {
		t.Fatalf("ignored REST recovery failed: %+v, %v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "file.txt")); err != nil || string(got) != contents {
		t.Fatalf("ignored REST promoted corrupt output: %q, %v", got, err)
	}
	_, offsets := s.retrievals()
	if len(offsets) < 3 || offsets[len(offsets)-1] != 0 {
		t.Fatalf("ignored REST was not retried from zero: %v", offsets)
	}
}

func TestWireResumeFinalFTPFailurePreservesFinal(t *testing.T) {
	const contents = "full payload with failed completion"
	s, url := newWireFTP(t, map[string]string{"/file.txt": contents}, "ftp-fail")
	dest := t.TempDir()
	final := filepath.Join(dest, "file.txt")
	if err := os.WriteFile(final, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
	if err == nil || result.Failed != 1 || result.ProgressBytes != 0 {
		t.Fatalf("failed completion accepted: %+v, %v", result, err)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != "original" {
		t.Fatalf("failed completion changed final: %q, %v", got, err)
	}
	result, err = Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
	if err == nil || result.Completed != 0 {
		t.Fatalf("full partial bypassed FTP completion: %+v, %v", result, err)
	}
	_, offsets := s.retrievals()
	if len(offsets) != 2 || offsets[1] != int64(len(contents)) {
		t.Fatalf("full partial was not validated with RETR: %v", offsets)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != "original" {
		t.Fatalf("full partial replaced final despite failed completion: %q, %v", got, err)
	}
}

func TestWireResumeCancellationPreservesPartial(t *testing.T) {
	contents := strings.Repeat("data", 128*1024)
	s, url := newWireFTP(t, map[string]string{"/file.txt": contents}, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dest := t.TempDir()
	final := filepath.Join(dest, "file.txt")
	if err := os.WriteFile(final, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	progressed := make(chan struct{})
	var once sync.Once
	go func() {
		_, err := Run(ctx, Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Timeout: time.Minute, Resume: true}, func(p Snapshot) {
			if p.Bytes > 0 {
				once.Do(func() { close(progressed) })
			}
		})
		done <- err
	}()
	select {
	case <-progressed:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("partial data was not written")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt data socket")
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != "original" {
		t.Fatalf("cancellation changed final: %q, %v", got, err)
	}
	s.setMode("")
	result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, Resume: true}, nil)
	if err != nil || result.Completed != 1 {
		t.Fatalf("preserved partial did not resume: %+v, %v", result, err)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != contents {
		t.Fatalf("resumed cancellation output incorrect: %q, %v", got, err)
	}
	_, offsets := s.retrievals()
	if len(offsets) != 2 || offsets[1] <= 0 || offsets[1] > int64(len(contents)) {
		t.Fatalf("cancellation partial was not retained: %v", offsets)
	}
	if result.Resumed != 1 || result.Bytes != int64(len(contents))-offsets[1] {
		t.Fatalf("cancelled prefix was not credited correctly: %+v, offset %d", result, offsets[1])
	}
}

func TestWireResumeDryRunAndSkipExisting(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		s, url := newWireFTP(t, map[string]string{"/file.txt": "contents"}, "")
		dest := t.TempDir()
		result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, DryRun: true, Resume: true}, nil)
		if err != nil || result.Discovered != 1 || result.Completed != 0 {
			t.Fatalf("dry run failed: %+v, %v", result, err)
		}
		entries, err := os.ReadDir(dest)
		if err != nil || len(entries) != 0 {
			t.Fatalf("dry run created disk state: %v, %v", entries, err)
		}
		requests, _ := s.retrievals()
		if len(requests) != 0 {
			t.Fatalf("dry run retrieved data: %v", requests)
		}
	})
	t.Run("skip existing", func(t *testing.T) {
		const contents = "contents"
		s, url := newWireFTP(t, map[string]string{"/file.txt": contents}, "")
		dest := t.TempDir()
		if err := os.WriteFile(filepath.Join(dest, "file.txt"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := Run(context.Background(), Config{URL: url + "/file.txt", Destination: dest, Connections: 1, SkipExisting: true, Resume: true}, nil)
		if err != nil || result.Skipped != 1 || result.Completed != 0 {
			t.Fatalf("skip-existing lost precedence: %+v, %v", result, err)
		}
		requests, _ := s.retrievals()
		if len(requests) != 0 {
			t.Fatalf("skip-existing retrieved data: %v", requests)
		}
	})
}

func TestWireProtectsResumeNamespace(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			s, url := newWireFTP(t, map[string]string{"/tree/.jesusftp-resume/foreign.part": "intruder"}, "")
			result, err := Run(context.Background(), Config{URL: url + "/tree", Destination: t.TempDir(), Connections: 1, Resume: resume}, nil)
			if err == nil || result.Failed != 1 || result.Completed != 0 {
				t.Fatalf("reserved destination accepted: %+v, %v", result, err)
			}
			requests, _ := s.retrievals()
			if len(requests) != 0 {
				t.Fatalf("reserved destination retrieved: %v", requests)
			}
		})
	}
}
