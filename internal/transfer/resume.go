package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const resumeDir = ".jesusftp-resume"

const resumeMetadataVersion = 1

type resumeStore struct {
	root *os.Root
	lock *os.File

	mu     sync.Mutex
	active map[string]struct{}
	closed bool
}

type resumeMetadata struct {
	Version int    `json:"version"`
	Source  string `json:"source"`
	Size    int64  `json:"size"`
	ModTime string `json:"modtime"`
}

type partialFile struct {
	File   *os.File
	Path   string
	Offset int64

	store    *resumeStore
	local    string
	metadata string

	mu     sync.Mutex
	closed bool
}

// openResumeStore opens the private, destination-relative resume area and
// holds its process-wide lock until Close.
func openResumeStore(root *os.Root) (*resumeStore, error) {
	if root == nil {
		return nil, errors.New("nil destination root")
	}

	info, err := root.Lstat(resumeDir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := root.Mkdir(resumeDir, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create resume directory: %w", err)
		}
		info, err = root.Lstat(resumeDir)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect resume directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("resume directory is not a directory")
	}

	resumeRoot, err := root.OpenRoot(resumeDir)
	if err != nil {
		return nil, fmt.Errorf("open resume directory: %w", err)
	}
	opened, err := resumeRoot.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		resumeRoot.Close()
		return nil, errors.New("resume directory changed while opening it")
	}
	if err := resumeRoot.Chmod(".", 0700); err != nil {
		resumeRoot.Close()
		return nil, fmt.Errorf("protect resume directory: %w", err)
	}

	lock, err := openRegular(resumeRoot, ".lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		resumeRoot.Close()
		return nil, fmt.Errorf("open resume lock: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		resumeRoot.Close()
		return nil, fmt.Errorf("lock resume directory: %w", err)
	}

	return &resumeStore{root: resumeRoot, lock: lock, active: make(map[string]struct{})}, nil
}

func (s *resumeStore) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	lock := s.lock
	root := s.root
	s.mu.Unlock()

	var err error
	if lock != nil {
		if unlockErr := unix.Flock(int(lock.Fd()), unix.LOCK_UN); unlockErr != nil {
			err = unlockErr
		}
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}
	if root != nil {
		if closeErr := root.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func (s *resumeStore) prepare(source string, j job) (*partialFile, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("resume store is closed")
	}
	if _, ok := s.active[j.local]; ok {
		s.mu.Unlock()
		return nil, errors.New("resume file is already active")
	}
	s.active[j.local] = struct{}{}
	s.mu.Unlock()

	fail := func(err error) (*partialFile, error) {
		s.release(j.local)
		return nil, err
	}

	hash := sha256.Sum256([]byte(j.local))
	name := hex.EncodeToString(hash[:])
	partName := name + ".part"
	metadataName := name + ".json"
	file, err := openRegular(s.root, partName, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fail(fmt.Errorf("open resume data: %w", err))
	}

	partial := &partialFile{
		File:     file,
		Path:     resumeDir + "/" + partName,
		store:    s,
		local:    j.local,
		metadata: metadataName,
	}
	metadata, metadataOK, err := s.readMetadata(metadataName)
	if err != nil {
		file.Close()
		return fail(err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fail(fmt.Errorf("stat resume data: %w", err))
	}
	valid := metadataOK && metadata.matches(source, j) && info.Size() >= 0 && info.Size() <= j.size
	if !valid {
		if err := partial.Reset(); err != nil {
			file.Close()
			return fail(err)
		}
		if err := s.writeMetadata(metadataName, resumeMetadataFor(source, j)); err != nil {
			file.Close()
			return fail(err)
		}
		return partial, nil
	}

	if _, err := file.Seek(info.Size(), io.SeekStart); err != nil {
		file.Close()
		return fail(fmt.Errorf("seek resume data: %w", err))
	}
	partial.Offset = info.Size()
	return partial, nil
}

func (s *resumeStore) release(local string) {
	s.mu.Lock()
	delete(s.active, local)
	s.mu.Unlock()
}

func (p *partialFile) Reset() error {
	if err := p.File.Truncate(0); err != nil {
		return fmt.Errorf("reset resume data: %w", err)
	}
	if _, err := p.File.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek reset resume data: %w", err)
	}
	p.Offset = 0
	return nil
}

func (p *partialFile) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	err := p.File.Close()
	p.store.release(p.local)
	return err
}

// Finish removes the metadata only after the caller has atomically promoted
// the partial data to its final destination.
func (p *partialFile) Finish() {
	_ = p.store.root.Remove(p.metadata)
}

func (m resumeMetadata) matches(source string, j job) bool {
	if m.Version != resumeMetadataVersion || m.Source != source || m.Size != j.size || j.mtime.IsZero() {
		return false
	}
	return m.ModTime == j.mtime.UTC().Format(time.RFC3339Nano)
}

func resumeMetadataFor(source string, j job) resumeMetadata {
	return resumeMetadata{
		Version: resumeMetadataVersion,
		Source:  source,
		Size:    j.size,
		ModTime: j.mtime.UTC().Format(time.RFC3339Nano),
	}
}

func (s *resumeStore) readMetadata(name string) (resumeMetadata, bool, error) {
	info, err := s.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return resumeMetadata{}, false, nil
	}
	if err != nil {
		return resumeMetadata{}, false, fmt.Errorf("inspect resume metadata: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return resumeMetadata{}, false, errors.New("resume metadata is not a regular file")
	}

	file, err := s.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return resumeMetadata{}, false, fmt.Errorf("open resume metadata: %w", err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return resumeMetadata{}, false, fmt.Errorf("stat resume metadata: %w", err)
	} else if !info.Mode().IsRegular() {
		return resumeMetadata{}, false, errors.New("resume metadata is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return resumeMetadata{}, false, fmt.Errorf("read resume metadata: %w", err)
	}
	if len(data) > 1<<20 {
		return resumeMetadata{}, false, nil
	}
	var metadata resumeMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return resumeMetadata{}, false, nil
	}
	return metadata, true, nil
}

func (s *resumeStore) writeMetadata(name string, metadata resumeMetadata) error {
	if info, err := s.root.Lstat(name); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("resume metadata is not a regular file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect resume metadata: %w", err)
	}

	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode resume metadata: %w", err)
	}
	temporary := name + ".tmp"
	if info, err := s.root.Lstat(temporary); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("resume metadata temporary is not a regular file")
		}
		if err := s.root.Remove(temporary); err != nil {
			return fmt.Errorf("remove stale resume metadata: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect resume metadata temporary: %w", err)
	}
	file, err := openRegular(s.root, temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create resume metadata: %w", err)
	}
	defer func() {
		file.Close()
		_ = s.root.Remove(temporary)
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write resume metadata: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close resume metadata: %w", err)
	}
	if err := s.root.Rename(temporary, name); err != nil {
		return fmt.Errorf("replace resume metadata: %w", err)
	}
	return nil
}

func openRegular(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
	info, err := root.Lstat(name)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("resume member is not a regular file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	file, err := root.OpenFile(name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, perm)
	if err != nil {
		return nil, err
	}
	info, err = file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("resume member is not a regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		file.Close()
		return nil, errors.New("resume member must not have hard links")
	}
	if err := file.Chmod(perm); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
