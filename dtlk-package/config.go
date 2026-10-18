package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

const defaultServer = "https://dtlk.ga111o.com"

// configPath is the file install.sh writes: one "server=<url>" line.
func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "dtlk", "config")
}

// serverURL resolves the server: DTLK_SERVER env, then the config file, then the default.
func serverURL() (string, error) {
	if v := strings.TrimSpace(os.Getenv("DTLK_SERVER")); v != "" {
		return normalizeServer(v), nil
	}
	f, err := os.Open(configPath())
	if os.IsNotExist(err) {
		return defaultServer, nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && strings.TrimSpace(key) == "server" && strings.TrimSpace(val) != "" {
			return normalizeServer(val), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return defaultServer, nil
}

// normalizeServer adds https:// when no scheme is given and drops trailing slashes.
func normalizeServer(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	return s
}
