package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var configMu sync.Mutex

type connectionConfig struct {
	URL  string `json:"url"`
	User string `json:"user,omitempty"`
}

type connectionFile struct {
	Connections map[string]connectionConfig `json:"connections"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find configuration directory: %w", err)
	}
	return filepath.Join(dir, "jesusftp", "config.json"), nil
}

func validAlias(name string) bool {
	if name == "config" || name == "" || !asciiLetter(name[0]) {
		return false
	}
	for i := range name[1:] {
		c := name[i+1]
		if !asciiLetter(c) && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func asciiLetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

func validateConnection(c connectionConfig) error {
	if err := validURL(c.URL); err != nil {
		return err
	}
	u, _ := url.Parse(c.URL)
	if strings.ContainsAny(u.Path, "\r\n\x00") || strings.ContainsAny(c.User, "\r\n\x00") {
		return errors.New("connection paths and usernames cannot contain line breaks or NUL characters")
	}
	if u.User != nil {
		if strings.ContainsAny(u.User.Username(), "\r\n\x00") {
			return errors.New("connection usernames cannot contain line breaks or NUL characters")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return errors.New("connection URLs must not contain passwords")
		}
	}
	return nil
}

func loadConnections(path string) (connectionFile, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return connectionFile{Connections: make(map[string]connectionConfig)}, nil
	}
	if err != nil {
		return connectionFile{}, fmt.Errorf("open connection config: %w", err)
	}
	defer f.Close()

	var cfg connectionFile
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return connectionFile{}, errors.New("read connection config: malformed JSON or unsupported fields")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return connectionFile{}, errors.New("read connection config: unexpected content after JSON object")
	}
	if cfg.Connections == nil {
		return connectionFile{}, errors.New("read connection config: a connections object is required")
	}
	for name, connection := range cfg.Connections {
		if !validAlias(name) {
			return connectionFile{}, errors.New("read connection config: invalid connection alias")
		}
		if err := validateConnection(connection); err != nil {
			return connectionFile{}, fmt.Errorf("read connection config: %w", err)
		}
	}
	return cfg, nil
}

func writeConnections(path string, cfg connectionFile) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode connection config: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("set configuration directory permissions: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("set configuration file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write connection config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync connection config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close connection config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace connection config: %w", err)
	}
	return nil
}

// mutateConnections serializes a read-modify-write operation both in this
// process and with other jesusftp processes sharing the same configuration.
func mutateConnections(path string, change func(map[string]connectionConfig) error) error {
	configMu.Lock()
	defer configMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open configuration lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock connection config: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	cfg, err := loadConnections(path)
	if err != nil {
		return err
	}
	if err := change(cfg.Connections); err != nil {
		return err
	}
	return writeConnections(path, cfg)
}

func resolveConnection(target, overrideUser string) (rawURL, username string, err error) {
	if strings.Contains(target, "://") {
		return target, overrideUser, nil
	}
	if !validAlias(target) {
		return "", "", errors.New("connection name must begin with a letter and use only letters, numbers, _ or -")
	}
	path, err := configPath()
	if err != nil {
		return "", "", err
	}
	cfg, err := loadConnections(path)
	if err != nil {
		return "", "", err
	}
	connection, ok := cfg.Connections[target]
	if !ok {
		return "", "", fmt.Errorf("unknown connection alias %q; see 'jesusftp config list'", target)
	}
	if overrideUser != "" {
		return connection.URL, overrideUser, nil
	}
	return connection.URL, connection.User, nil
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, configHelp)
		return 0
	}
	command := args[0]
	if command == "path" {
		if len(args) != 1 {
			return configError(stderr, "config path accepts no arguments")
		}
		path, err := configPath()
		if err != nil {
			return configError(stderr, err.Error())
		}
		fmt.Fprintln(stdout, path)
		return 0
	}
	if command == "list" {
		if len(args) != 1 {
			return configError(stderr, "config list accepts no arguments")
		}
		path, err := configPath()
		if err != nil {
			return configError(stderr, err.Error())
		}
		cfg, err := loadConnections(path)
		if err != nil {
			return configError(stderr, err.Error())
		}
		names := make([]string, 0, len(cfg.Connections))
		for name := range cfg.Connections {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			fmt.Fprintln(stdout, "No saved connections. Add one with: jesusftp config add NAME URL")
			return 0
		}
		for _, name := range names {
			c := cfg.Connections[name]
			if c.User == "" {
				fmt.Fprintf(stdout, "%s\t%s\n", name, safeText(c.URL))
			} else {
				fmt.Fprintf(stdout, "%s\t%s\t%s\n", name, safeText(c.URL), safeText(c.User))
			}
		}
		return 0
	}
	if command == "show" || command == "remove" {
		if len(args) != 2 || !validAlias(args[1]) {
			return configError(stderr, "config show and remove require one valid connection alias")
		}
		name := args[1]
		path, err := configPath()
		if err != nil {
			return configError(stderr, err.Error())
		}
		if command == "show" {
			cfg, err := loadConnections(path)
			if err != nil {
				return configError(stderr, err.Error())
			}
			c, ok := cfg.Connections[name]
			if !ok {
				return configError(stderr, fmt.Sprintf("connection alias %q does not exist", name))
			}
			if c.User == "" {
				fmt.Fprintf(stdout, "%s\t%s\n", name, safeText(c.URL))
			} else {
				fmt.Fprintf(stdout, "%s\t%s\t%s\n", name, safeText(c.URL), safeText(c.User))
			}
			return 0
		}
		err = mutateConnections(path, func(connections map[string]connectionConfig) error {
			if _, ok := connections[name]; !ok {
				return fmt.Errorf("connection alias %q does not exist", name)
			}
			delete(connections, name)
			return nil
		})
		if err != nil {
			return configError(stderr, err.Error())
		}
		fmt.Fprintf(stdout, "Removed connection %s.\n", name)
		return 0
	}
	if command != "add" && command != "set" {
		return configError(stderr, "unknown config command; see 'jesusftp config --help'")
	}
	name, rawURL, user, err := configAddSetArgs(args[1:])
	if err != nil {
		return configError(stderr, err.Error())
	}
	if !validAlias(name) {
		return configError(stderr, "connection name must begin with a letter and use only letters, numbers, _ or -")
	}
	connection := connectionConfig{URL: rawURL, User: user}
	if err := validateConnection(connection); err != nil {
		return configError(stderr, err.Error())
	}
	path, err := configPath()
	if err != nil {
		return configError(stderr, err.Error())
	}
	err = mutateConnections(path, func(connections map[string]connectionConfig) error {
		_, exists := connections[name]
		if command == "add" && exists {
			return fmt.Errorf("connection alias %q already exists; use 'config set' to replace it", name)
		}
		if command == "set" && !exists {
			return fmt.Errorf("connection alias %q does not exist; use 'config add' to create it", name)
		}
		connections[name] = connection
		return nil
	})
	if err != nil {
		return configError(stderr, err.Error())
	}
	if command == "add" {
		fmt.Fprintf(stdout, "Saved connection %s. Connect with: jesusftp %s\n", name, name)
	} else {
		fmt.Fprintf(stdout, "Updated connection %s.\n", name)
	}
	return 0
}

func configAddSetArgs(args []string) (name, rawURL, user string, err error) {
	var positional []string
	literal := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !literal && a == "--" {
			literal = true
			continue
		}
		if !literal && a == "--user" {
			if i+1 == len(args) {
				return "", "", "", errors.New("--user needs a username")
			}
			i++
			user = args[i]
			continue
		}
		if !literal && strings.HasPrefix(a, "--user=") {
			user = strings.TrimPrefix(a, "--user=")
			continue
		}
		if !literal && strings.HasPrefix(a, "-") {
			return "", "", "", errors.New("unrecognized config option; see 'jesusftp config --help'")
		}
		positional = append(positional, a)
	}
	if len(positional) != 2 {
		return "", "", "", errors.New("add and set require NAME and URL")
	}
	return positional[0], positional[1], user, nil
}

func configError(stderr io.Writer, text string) int {
	fmt.Fprintf(stderr, "jesusftp: %s\n", safeText(text))
	return 2
}

const configHelp = `Connection aliases — the Book of Connections

Usage:
  jesusftp config add NAME URL [--user USER]
  jesusftp config set NAME URL [--user USER]
  jesusftp config list
  jesusftp config show NAME
  jesusftp config remove NAME
  jesusftp config path

A name begins with an ASCII letter and may then use letters, numbers, _ or -.
The name config is reserved. URLs may be ftp:// or ftps://, without passwords.
add creates an alias; set replaces its URL and optional username.
list shows saved aliases; show displays one; remove deletes one.
path prints the configuration file location. Passwords are prompted, never saved.
`
