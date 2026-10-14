package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func useConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	return home
}

func configCall(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runConfig(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestConfigRoundTripAndResolve(t *testing.T) {
	useConfigHome(t)
	if code, _, errout := configCall("list"); code != 0 {
		t.Fatalf("list absent book code = %d, stderr = %q", code, errout)
	}
	if code, _, errout := configCall("add", "--user", "noah", "ark", "ftps://host/private"); code != 0 {
		t.Fatalf("add code = %d, stderr = %q", code, errout)
	}
	if code, out, errout := configCall("list"); code != 0 || !strings.Contains(out, "ark\tftps://host/private\tnoah") {
		t.Fatalf("list code = %d, stdout = %q, stderr = %q", code, out, errout)
	}
	if code, out, errout := configCall("show", "ark"); code != 0 || !strings.Contains(out, "ftps://host/private") {
		t.Fatalf("show code = %d, stdout = %q, stderr = %q", code, out, errout)
	}
	url, user, err := resolveConnection("ark", "")
	if err != nil || url != "ftps://host/private" || user != "noah" {
		t.Fatalf("resolve saved = (%q, %q, %v)", url, user, err)
	}
	url, user, err = resolveConnection("ark", "shem")
	if err != nil || url != "ftps://host/private" || user != "shem" {
		t.Fatalf("resolve override = (%q, %q, %v)", url, user, err)
	}
	if code, _, errout := configCall("set", "ark", "ftp://host/renewed"); code != 0 {
		t.Fatalf("set code = %d, stderr = %q", code, errout)
	}
	_, user, err = resolveConnection("ark", "")
	if err != nil || user != "" {
		t.Fatalf("set did not replace username: %q, %v", user, err)
	}
	if code, _, errout := configCall("remove", "ark"); code != 0 {
		t.Fatalf("remove code = %d, stderr = %q", code, errout)
	}
	if _, _, err := resolveConnection("ark", ""); err == nil {
		t.Fatal("removed name resolved")
	}
}

func TestConfigRefusesPasswordsAndPreservesMalformedBook(t *testing.T) {
	home := useConfigHome(t)
	secret := "ftp://noah:secret@host/private"
	if code, _, errout := configCall("add", "ark", secret); code != 2 || strings.Contains(errout, "secret") {
		t.Fatalf("password URL code = %d, stderr = %q", code, errout)
	}
	path := filepath.Join(home, "jesusftp", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"connections":{"ark":{"url":"ftp://host","password":"secret"}}}`)
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, errout := configCall("add", "dove", "ftp://host/dove"); code != 2 || strings.Contains(errout, "secret") {
		t.Fatalf("malformed book code = %d, stderr = %q", code, errout)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, bad) {
		t.Fatalf("malformed book changed: %q, %v", got, err)
	}
}

func TestConfigPermissionsAndConcurrentMutations(t *testing.T) {
	home := useConfigHome(t)
	const names = 12
	var wg sync.WaitGroup
	errs := make(chan string, names)
	for i := range names {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a' + i))
			if code, _, errout := configCall("add", name, "ftp://host/"+name); code != 0 {
				errs <- errout
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for errout := range errs {
		t.Errorf("concurrent add: %s", errout)
	}
	path := filepath.Join(home, "jesusftp", "config.json")
	cfg, err := loadConnections(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Connections) != names {
		t.Fatalf("connections = %d, want %d", len(cfg.Connections), names)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0600 {
		t.Fatalf("config permissions = %#o", fileInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions = %#o", dirInfo.Mode().Perm())
	}
}

func TestResolveDirectURLDoesNotReadConfig(t *testing.T) {
	home := useConfigHome(t)
	path := filepath.Join(home, "jesusftp", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not valid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	url, user, err := resolveConnection("ftps://host/private", "noah")
	if err != nil || url != "ftps://host/private" || user != "noah" {
		t.Fatalf("resolve = (%q, %q, %v)", url, user, err)
	}
}

func TestConfigRejectsInvalidDestinationsWithoutReplacingSavedConnection(t *testing.T) {
	home := useConfigHome(t)
	if code, _, errout := configCall("add", "ark", "ftps://host/private", "--user", "noah"); code != 0 {
		t.Fatalf("add: %d %s", code, errout)
	}
	path := filepath.Join(home, "jesusftp", "config.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"ftp://noah:@host", "sftp://host", "ftp://:21", "ftp://host/path?query", "ftp://host/path#fragment", "ftp://host/%00"} {
		if code, _, _ := configCall("set", "ark", target); code != 2 {
			t.Errorf("accepted invalid destination %q: code %d", target, code)
		}
	}
	if code, _, _ := configCall("add", "ark", "ftp://different"); code != 2 {
		t.Error("duplicate add replaced existing connection")
	}
	if code, _, _ := configCall("set", "missing", "ftp://different"); code != 2 {
		t.Error("set unexpectedly created a new connection")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected mutation altered existing connections: %v", err)
	}
}

func TestConfigRejectsNullOrMissingConnections(t *testing.T) {
	home := useConfigHome(t)
	path := filepath.Join(home, "jesusftp", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"null", "{}", `{"connections":null}`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := configCall("add", "ark", "ftp://host"); code != 2 {
			t.Fatalf("accepted invalid configuration %s", content)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("rewrote invalid configuration: %v", err)
		}
	}
}
