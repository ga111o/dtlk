package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFilesArg(t *testing.T) {
	ok := []struct{ arg, server, tag string }{
		{"myproj", "", "myproj"},
		{" MyProj ", "", "myproj"},
		{"https://dtlk.ga111o.com/files/MyProj", "https://dtlk.ga111o.com", "myproj"},
		{"dtlk.ga111o.com/files/a_b-1/", "https://dtlk.ga111o.com", "a_b-1"},
		{"http://localhost:8000/dtlk/files/x", "http://localhost:8000/dtlk", "x"},
	}
	for _, c := range ok {
		server, tag, err := parseFilesArg(c.arg)
		if err != nil || server != c.server || tag != c.tag {
			t.Errorf("%q: got %q %q %v", c.arg, server, tag, err)
		}
	}
	bad := []string{
		"", " ", "a.b", "한글", strings.Repeat("x", 65),
		"https://dtlk.ga111o.com/myproj", "https://dtlk.ga111o.com/files", "https://dtlk.ga111o.com/files/",
		"https://files/myproj", "https://dtlk.ga111o.com/123456", "https://dtlk.ga111o.com/files/a.b",
		"https://dtlk.ga111o.com/files/myproj?x=1", "https://dtlk.ga111o.com/files/myproj#top", "ftp://dtlk.ga111o.com/files/myproj",
	}
	for _, arg := range bad {
		if server, tag, err := parseFilesArg(arg); err == nil {
			t.Errorf("%q: expected error, got %q %q", arg, server, tag)
		}
	}
}

// captureStdout runs f and returns what it printed to stdout.
func captureStdout(t *testing.T, f func() error) (string, error) {
	t.Helper()
	tmp, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	stdout := os.Stdout
	os.Stdout = tmp
	err = f()
	os.Stdout = stdout
	return readFile(t, tmp.Name()), err
}

func TestCmdFiles(t *testing.T) {
	path := setConfigHome(t)
	exp := time.Date(2026, 10, 26, 14, 20, 0, 0, time.UTC)
	files := []tagFile{
		{Code: "482913", Name: "report.pdf", Kind: "file", Size: 1258291, ExpiresAt: exp},
		{Code: "771204", Name: "photos", Kind: "dir", Size: 512, ExpiresAt: exp},
		{Code: "109385", Name: "dtlk-261025-180012", Kind: "bundle", Size: 12 << 10, ExpiresAt: exp},
		{Code: "555555", Name: "\x1b]52;c;evil\a", Kind: "file", Size: 1, ExpiresAt: exp},
	}
	mux := http.NewServeMux()
	for _, prefix := range []string{"", "/dtlk"} {
		mux.HandleFunc("GET "+prefix+"/api/tags/myproj", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"files": files})
		})
	}
	mux.HandleFunc("GET /api/tags/nope", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"detail": "List not found or all its files have expired"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	when := exp.Local().Format("2006-01-02 15:04")
	want := "CODE    EXPIRES           SIZE     NAME\n" +
		"482913  " + when + "  1.2 MB   report.pdf\n" +
		"771204  " + when + "  512 B    photos/\n" +
		"109385  " + when + "  12.0 KB  dtlk-261025-180012/\n" +
		"555555  " + when + "  1 B      \"\\x1b]52;c;evil\\a\"\n"

	// No argument: the configured tag on the configured server.
	t.Setenv("DTLK_SERVER", srv.URL)
	mustWrite(t, path, "tag=MyProj\n", 0o644)
	got, err := captureStdout(t, func() error { return cmdFiles(context.Background(), nil) })
	if err != nil || got != want {
		t.Fatalf("configured tag: %v\n%s", err, got)
	}

	// A List: link names its own server.
	t.Setenv("DTLK_SERVER", "http://127.0.0.1:1")
	for _, prefix := range []string{"", "/dtlk"} {
		got, err = captureStdout(t, func() error { return cmdFiles(context.Background(), []string{srv.URL + prefix + "/files/MyProj"}) })
		if err != nil || got != want {
			t.Fatalf("prefix %q: %v\n%s", prefix, err, got)
		}
	}

	_, err = captureStdout(t, func() error { return cmdFiles(context.Background(), []string{srv.URL + "/files/nope"}) })
	if err == nil || err.Error() != "List not found or all its files have expired" {
		t.Fatalf("unknown tag: %v", err)
	}

	mustWrite(t, path, "", 0o644)
	var ue *usageError
	for _, args := range [][]string{nil, {"a", "b"}, {""}} {
		if err := cmdFiles(context.Background(), args); !errors.As(err, &ue) {
			t.Errorf("%q: expected usage error, got %v", args, err)
		}
	}
}
