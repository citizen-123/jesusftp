package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestResumeStore(t *testing.T) (*os.Root, *resumeStore) {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := openResumeStore(root)
	if err != nil {
		root.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = root.Close()
	})
	return root, store
}

func testJob(local string) job {
	return job{local: local, size: 9, mtime: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
}

func resumeMember(local, suffix string) string {
	hash := sha256.Sum256([]byte(local))
	return hex.EncodeToString(hash[:]) + suffix
}

func TestResumePrefixReuse(t *testing.T) {
	_, store := newTestResumeStore(t)
	j := testJob("nested/file")
	first, err := store.prepare("identity-a", j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.File.Write([]byte("prefix")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := store.prepare("identity-a", j)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Offset != int64(len("prefix")) {
		t.Fatalf("offset = %d, want %d", second.Offset, len("prefix"))
	}
	if position, err := second.File.Seek(0, 1); err != nil || position != second.Offset {
		t.Fatalf("position = %d, %v", position, err)
	}
}

func TestResumeMismatchesAndMissingTimestampRestart(t *testing.T) {
	_, store := newTestResumeStore(t)
	base := testJob("file")
	for _, tc := range []struct {
		name   string
		source string
		job    job
	}{
		{name: "source", source: "identity-b", job: base},
		{name: "size", source: "identity-a", job: func() job { j := base; j.size++; return j }()},
		{name: "mtime", source: "identity-a", job: func() job { j := base; j.mtime = j.mtime.Add(time.Second); return j }()},
		{name: "no timestamp", source: "identity-a", job: func() job { j := base; j.mtime = time.Time{}; return j }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, err := store.prepare("identity-a", base)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := first.File.Write([]byte("old")); err != nil {
				t.Fatal(err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			second, err := store.prepare(tc.source, tc.job)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			if second.Offset != 0 {
				t.Fatalf("offset = %d, want reset", second.Offset)
			}
			if info, err := second.File.Stat(); err != nil || info.Size() != 0 {
				t.Fatalf("size = %v, %v", info.Size(), err)
			}
		})
	}
}

func TestResumeMalformedMissingAndOversizedRestart(t *testing.T) {
	_, store := newTestResumeStore(t)
	j := testJob("file")
	writePartial := func(t *testing.T, content string) {
		t.Helper()
		p, err := store.prepare("identity", j)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.File.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertReset := func(t *testing.T) {
		t.Helper()
		p, err := store.prepare("identity", j)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		if p.Offset != 0 {
			t.Fatalf("offset = %d, want zero", p.Offset)
		}
	}

	writePartial(t, "old")
	if err := store.root.WriteFile(resumeMember(j.local, ".json"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	assertReset(t)

	if err := store.root.Remove(resumeMember(j.local, ".json")); err != nil {
		t.Fatal(err)
	}
	writePartial(t, "old")
	if err := store.root.Remove(resumeMember(j.local, ".json")); err != nil {
		t.Fatal(err)
	}
	assertReset(t)

	writePartial(t, strings.Repeat("x", int(j.size+1)))
	assertReset(t)
}

func TestResumeSupportsLongLocalName(t *testing.T) {
	_, store := newTestResumeStore(t)
	j := testJob(strings.Repeat("x", 255))
	p, err := store.prepare("identity", j)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if len(p.Path) >= 255 {
		t.Fatalf("part path too long: %d", len(p.Path))
	}
}

func TestResumeStoreExclusiveAndDuplicateActive(t *testing.T) {
	root, store := newTestResumeStore(t)
	if _, err := openResumeStore(root); err == nil {
		t.Fatal("second resume store unexpectedly acquired lock")
	}
	j := testJob("file")
	p, err := store.prepare("identity", j)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := store.prepare("identity", j); err == nil {
		t.Fatal("duplicate active partial unexpectedly opened")
	}
}

func TestResumeRefusesSymlinks(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		if err := root.Symlink(".", resumeDir); err != nil {
			t.Fatal(err)
		}
		if _, err := openResumeStore(root); err == nil {
			t.Fatal("symlinked resume directory was accepted")
		}
	})
	t.Run("data", func(t *testing.T) {
		_, store := newTestResumeStore(t)
		j := testJob("file")
		if err := store.root.Symlink("elsewhere", resumeMember(j.local, ".part")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.prepare("identity", j); err == nil {
			t.Fatal("symlinked resume data was accepted")
		}
	})
}

func TestResumeRefusesLinkedMetadataAndData(t *testing.T) {
	_, store := newTestResumeStore(t)
	j := testJob("file")
	if err := store.root.WriteFile("valuable", []byte("do not touch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.root.Link("valuable", resumeMember(j.local, ".part")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.prepare("identity", j); err == nil {
		t.Fatal("hard-linked partial was accepted")
	}
	got, err := store.root.ReadFile("valuable")
	if err != nil || string(got) != "do not touch" {
		t.Fatalf("hard-link target changed: %q %v", got, err)
	}
	if err := store.root.Remove(resumeMember(j.local, ".part")); err != nil {
		t.Fatal(err)
	}
	if err := store.root.Symlink("valuable", resumeMember(j.local, ".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.prepare("identity", j); err == nil {
		t.Fatal("symlinked metadata was accepted")
	}
}
