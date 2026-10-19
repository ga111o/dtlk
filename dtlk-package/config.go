package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const defaultServer = "https://dtlk.ga111o.com"

// configKeys are the settings `dtlk config` shows and sets, in display order.
var configKeys = []string{"server", "hours", "tag"}

// configPath is the config file: "key=value" lines. install.sh writes server=,
// `dtlk config` writes any of configKeys.
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

// readConfig parses the config file; a missing file is empty. The first non-empty value of a key wins.
func readConfig() (map[string]string, error) {
	cfg := map[string]string{}
	f, err := os.Open(configPath())
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if ok && val != "" && cfg[key] == "" {
			cfg[key] = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// writeConfig sets key=value in the config file, or removes the key when value is "".
// Other lines are kept; the key's first line is replaced in place and any duplicates dropped.
func writeConfig(key, value string) error {
	path := configPath()
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var lines []string
	if s := strings.TrimRight(string(data), "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	var out []string
	placed := value == ""
	for _, line := range lines {
		if k, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(k) == key {
			if !placed {
				out = append(out, key+"="+value)
				placed = true
			}
			continue
		}
		out = append(out, line)
	}
	if !placed {
		out = append(out, key+"="+value)
	}
	content := strings.Join(out, "\n")
	if content != "" {
		content += "\n"
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// serverURL resolves the server: DTLK_SERVER env, then the config file, then the default.
func serverURL() (string, error) {
	if v := strings.TrimSpace(os.Getenv("DTLK_SERVER")); v != "" {
		return normalizeServer(v), nil
	}
	cfg, err := readConfig()
	if err != nil {
		return "", err
	}
	if v := cfg["server"]; v != "" {
		return normalizeServer(v), nil
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

// normalizeConfig validates value for key and returns it as stored; "" stays "" (unset).
func normalizeConfig(key, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	switch key {
	case "server":
		return normalizeServer(value), nil
	case "hours":
		h, err := parseHours(value)
		return strconv.Itoa(h), err
	case "tag":
		return parseTag(value)
	}
	return "", unknownConfigKey(key)
}

func unknownConfigKey(key string) error {
	return usageErrorf("unknown config key: %s (keys: %s)", key, strings.Join(configKeys, ", "))
}

// effectiveConfig returns "key=value" for each key as dtlk will use it, defaults included.
func effectiveConfig(keys []string) ([]string, error) {
	cfg, err := readConfig()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := cfg[k]
		switch k {
		case "server":
			if env := strings.TrimSpace(os.Getenv("DTLK_SERVER")); env != "" {
				out = append(out, "server="+normalizeServer(env)+" (DTLK_SERVER)")
				continue
			}
			if v == "" {
				v = defaultServer
			} else {
				v = normalizeServer(v)
			}
		case "hours":
			if v == "" {
				v = strconv.Itoa(defaultHours)
			}
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// cmdConfig shows all keys, shows one key, or sets one ("" unsets it).
func cmdConfig(args []string) error {
	if len(args) > 2 {
		return usageErrorf("usage: dtlk config [key [value]]  (keys: %s)", strings.Join(configKeys, ", "))
	}
	keys := configKeys
	if len(args) > 0 {
		if !slices.Contains(configKeys, args[0]) {
			return unknownConfigKey(args[0])
		}
		keys = args[:1]
	}
	if len(args) == 2 {
		v, err := normalizeConfig(args[0], args[1])
		if err != nil {
			return err
		}
		if err := writeConfig(args[0], v); err != nil {
			return fmt.Errorf("failed to write config file: %w", err)
		}
	}
	lines, err := effectiveConfig(keys)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}
