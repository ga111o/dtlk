package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseDownArg(t *testing.T) {
	ok := []struct{ arg, server, code string }{
		{"123456", "", "123456"},
		{" 123456 ", "", "123456"},
		{"https://dtlk.ga111o.com/123456", "https://dtlk.ga111o.com", "123456"},
		{"dtlk.ga111o.com/123456", "https://dtlk.ga111o.com", "123456"},
		{"https://dtlk.ga111o.com/123456/", "https://dtlk.ga111o.com", "123456"},
		{"HTTPS://dtlk.ga111o.com/123456", "https://dtlk.ga111o.com", "123456"},
		{"http://localhost:8000/123456", "http://localhost:8000", "123456"},
		{"localhost:8000/123456", "https://localhost:8000", "123456"},
		{"https://example.com/dtlk/123456", "https://example.com/dtlk", "123456"},
		{"example.com//123456", "https://example.com", "123456"},
	}
	for _, c := range ok {
		server, code, err := parseDownArg(c.arg)
		if err != nil || server != c.server || code != c.code {
			t.Errorf("%q: got %q %q %v", c.arg, server, code, err)
		}
	}
	bad := []string{
		"", "12345", "1234567", "abcdef", "12345a",
		"/123456", "https://123456", "https:///123456", "https://dtlk.ga111o.com", "https://dtlk.ga111o.com/",
		"https://dtlk.ga111o.com/12345", "https://dtlk.ga111o.com/files/mytag",
		"https://dtlk.ga111o.com/123456?x=1", "https://dtlk.ga111o.com/123456?", "https://dtlk.ga111o.com/123456#top",
		"ftp://dtlk.ga111o.com/123456", "https://host:port/123456",
	}
	for _, arg := range bad {
		if server, code, err := parseDownArg(arg); err == nil {
			t.Errorf("%q: expected error, got %q %q", arg, server, code)
		}
	}
}

// fakeServer serves one upload (info, data) under prefix, like the real /api/files routes.
func fakeServer(t *testing.T, prefix, code string, info fileInfo, data []byte) *httptest.Server {
	t.Helper()
	info.Size, info.ExpiresAt = int64(len(data)), time.Now().Add(time.Hour)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"/api/files/"+code, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(info)
	})
	mux.HandleFunc("GET "+prefix+"/api/files/"+code+"/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCmdDownUsesServerFromArg(t *testing.T) {
	setConfigHome(t)
	t.Setenv("DTLK_SERVER", "http://127.0.0.1:1") // must be ignored when the argument names a server

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "test1/hello.txt"), "hello", 0o644)
	data := packZstd(t, filepath.Join(src, "test1"), "test1")

	for _, prefix := range []string{"", "/dtlk"} {
		srv := fakeServer(t, prefix, "123456", fileInfo{Name: "test1", Kind: "dir"}, data)
		dest := t.TempDir()
		t.Chdir(dest)
		if err := cmdDown(context.Background(), []string{srv.URL + prefix + "/123456"}); err != nil {
			t.Fatalf("prefix %q: %v", prefix, err)
		}
		if got := readFile(t, filepath.Join(dest, "test1/hello.txt")); got != "hello" {
			t.Fatalf("prefix %q: got %q", prefix, got)
		}
	}
}

func TestCmdDownUnpacksBundle(t *testing.T) {
	setConfigHome(t)
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "linear-algebra3.pdf"), "three", 0o644)
	mustWrite(t, filepath.Join(src, "linear-algebra4.pdf"), "four", 0o644)
	mustWrite(t, filepath.Join(src, "notes/a.txt"), "a", 0o644)
	b, err := newBundle([]string{
		filepath.Join(src, "linear-algebra3.pdf"), filepath.Join(src, "linear-algebra4.pdf"), filepath.Join(src, "notes"),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data := packBundle(t, b)
	info := fileInfo{Name: b.name, Kind: "bundle", Items: b.items()}
	srv := fakeServer(t, "", "123456", info, data)

	dest := t.TempDir()
	t.Chdir(dest)
	if err := cmdDown(context.Background(), []string{srv.URL + "/123456"}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"linear-algebra3.pdf": "three", "linear-algebra4.pdf": "four", "notes/a.txt": "a"} {
		if got := readFile(t, filepath.Join(dest, rel)); got != want {
			t.Errorf("%s = %q", rel, got)
		}
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 3 {
		t.Fatalf("expected only the 3 items in %s, got %v", dest, entries)
	}

	// Without a terminal a name conflict fails before anything is saved.
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	stdin := os.Stdin
	os.Stdin = null
	t.Cleanup(func() { os.Stdin = stdin })
	dest = t.TempDir()
	t.Chdir(dest)
	mustWrite(t, filepath.Join(dest, "linear-algebra4.pdf"), "mine", 0o644)
	if err := cmdDown(context.Background(), []string{srv.URL + "/123456"}); err == nil || !strings.Contains(err.Error(), "linear-algebra4.pdf") {
		t.Fatalf("conflict: %v", err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 1 || readFile(t, filepath.Join(dest, "linear-algebra4.pdf")) != "mine" {
		t.Fatalf("conflict touched %s: %v", dest, entries)
	}

	// The archive must hold exactly the listed items.
	for _, items := range [][]bundleItem{info.Items[:2], append(info.Items[:2:2], bundleItem{"other", "file"})} {
		bad := fakeServer(t, "", "123456", fileInfo{Name: b.name, Kind: "bundle", Items: items}, data)
		dest = t.TempDir()
		t.Chdir(dest)
		if err := cmdDown(context.Background(), []string{bad.URL + "/123456"}); err == nil || !strings.Contains(err.Error(), "item list") {
			t.Fatalf("items %v: %v", items, err)
		}
		if entries, _ := os.ReadDir(dest); len(entries) != 0 {
			t.Fatalf("items %v: left %v", items, entries)
		}
	}
}

func TestPlanSavesRejectsBadItems(t *testing.T) {
	dir := t.TempDir()
	for _, items := range [][]bundleItem{nil, {{"a", "file"}, {"a", "file"}}, {{"a", "file"}, {"../x", "file"}}, {{"a", "file"}, {"", "dir"}}} {
		if _, err := planSaves(dir, &fileInfo{Name: "dtlk-261019-143005", Kind: "bundle", Items: items}, nil); err == nil {
			t.Errorf("%v: expected error", items)
		}
	}
}
