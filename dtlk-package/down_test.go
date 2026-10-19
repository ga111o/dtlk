package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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

// fakeServer serves one upload of src under prefix, like the real /api/files routes.
func fakeServer(t *testing.T, prefix, code, name string, data []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"/api/files/"+code, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(fileInfo{Name: name, Kind: "dir", Size: int64(len(data)), ExpiresAt: time.Now().Add(time.Hour)})
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
		srv := fakeServer(t, prefix, "123456", "test1", data)
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
