package main

import (
	"os"
	"path/filepath"
	"testing"
)

func setConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("DTLK_SERVER", "")
	return filepath.Join(dir, "dtlk", "config")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWriteConfig(t *testing.T) {
	path := setConfigHome(t)

	// creates the directory and file
	if err := writeConfig("tag", "asdf"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "tag=asdf\n" {
		t.Fatalf("new file: %q", got)
	}

	// replaces in place, drops duplicates, keeps comments and unknown keys
	mustWrite(t, path, "# mine\nserver=https://a.example\n tag = old\nfoo=bar\ntag=dup\n", 0o600)
	if err := writeConfig("tag", "new"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "# mine\nserver=https://a.example\ntag=new\nfoo=bar\n" {
		t.Fatalf("replace: %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}

	// appends a missing key
	if err := writeConfig("hours", "48"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "# mine\nserver=https://a.example\ntag=new\nfoo=bar\nhours=48\n" {
		t.Fatalf("append: %q", got)
	}

	// "" removes the key
	if err := writeConfig("tag", ""); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "# mine\nserver=https://a.example\nfoo=bar\nhours=48\n" {
		t.Fatalf("unset: %q", got)
	}

	cfg, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg["server"] != "https://a.example" || cfg["hours"] != "48" || cfg["tag"] != "" {
		t.Fatalf("readConfig: %v", cfg)
	}
}

func TestWriteConfigThroughSymlink(t *testing.T) {
	path := setConfigHome(t)
	real := filepath.Join(t.TempDir(), "dotfiles-config")
	mustWrite(t, real, "server=https://a.example\n", 0o644)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig("tag", "x"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, real); got != "server=https://a.example\ntag=x\n" {
		t.Fatalf("target: %q", got)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", info, err)
	}
}

func TestNormalizeConfig(t *testing.T) {
	ok := map[[2]string]string{
		{"server", "files.example.com/"}: "https://files.example.com",
		{"server", "http://x:8780"}:      "http://x:8780",
		{"hours", "48"}:                  "48",
		{"hours", " 7 "}:                 "7",
		{"tag", "MyProj"}:                "myproj",
		{"tag", ""}:                      "",
		{"hours", ""}:                    "",
		{"server", ""}:                   "",
	}
	for in, want := range ok {
		if got, err := normalizeConfig(in[0], in[1]); err != nil || got != want {
			t.Errorf("%v: got %q %v", in, got, err)
		}
	}
	for _, in := range [][2]string{{"hours", "0"}, {"hours", "169"}, {"hours", "x"}, {"tag", "a.b"}, {"name", "x"}} {
		if _, err := normalizeConfig(in[0], in[1]); err == nil {
			t.Errorf("%v: expected error", in)
		}
	}
}

func TestServerURLPrecedence(t *testing.T) {
	path := setConfigHome(t)
	if got, _ := serverURL(); got != defaultServer {
		t.Fatalf("default: %s", got)
	}
	mustWrite(t, path, "server=files.example.com\n", 0o644)
	if got, _ := serverURL(); got != "https://files.example.com" {
		t.Fatalf("file: %s", got)
	}
	t.Setenv("DTLK_SERVER", "http://127.0.0.1:8780/")
	if got, _ := serverURL(); got != "http://127.0.0.1:8780" {
		t.Fatalf("env: %s", got)
	}
}

func TestEffectiveConfig(t *testing.T) {
	path := setConfigHome(t)
	got, err := effectiveConfig(configKeys)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"server=" + defaultServer, "hours=24", "tag="}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("defaults: %q", got)
	}

	mustWrite(t, path, "server=a.example\nhours=48\ntag=asdf\n", 0o644)
	t.Setenv("DTLK_SERVER", "b.example")
	got, err = effectiveConfig(configKeys)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"server=https://b.example (DTLK_SERVER)", "hours=48", "tag=asdf"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("set: %q", got)
	}
}

func TestCmdConfigErrors(t *testing.T) {
	setConfigHome(t)
	for _, args := range [][]string{{"name"}, {"name", "x"}, {"tag", "a", "b"}, {"hours", "500"}, {"tag", "a.b"}} {
		if err := cmdConfig(args); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}
