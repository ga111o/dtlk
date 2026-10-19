package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func mustWrite(t *testing.T, path, content string, perm fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func packZstd(t *testing.T, root, top string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTar(context.Background(), zw, root, top, func(string) {}, nil); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func unpackZstd(t *testing.T, data []byte, dest string) (string, error) {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	return extractTar(zr, dest)
}

func TestRoundTripDirectory(t *testing.T) {
	src := t.TempDir()
	root := filepath.Join(src, "test1")
	mustWrite(t, filepath.Join(root, "hello/world/text1.txt"), "hello", 0o644)
	mustWrite(t, filepath.Join(root, "run.sh"), "#!/bin/sh\n", 0o755)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello/world/text1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	scan, err := scanSource(root+"/", "test1")
	if err != nil {
		t.Fatal(err)
	}
	if scan.Files != 2 || scan.Dirs != 4 || scan.Links != 1 || scan.Bytes != 15 {
		t.Fatalf("unexpected scan: %+v", scan)
	}

	data := packZstd(t, root, "test1")
	dest := t.TempDir()
	top, err := unpackZstd(t, data, dest)
	if err != nil {
		t.Fatal(err)
	}
	if top != "test1" {
		t.Fatalf("top = %q", top)
	}
	got, err := os.ReadFile(filepath.Join(dest, "test1/hello/world/text1.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("text1.txt = %q, %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "test1/run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh mode = %v, %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "test1/empty")); err != nil || !fi.IsDir() {
		t.Fatalf("empty dir missing: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "test1/link")); err != nil || target != "hello/world/text1.txt" {
		t.Fatalf("link = %q, %v", target, err)
	}
	if scan.TarSize < int64(len(data)) {
		t.Fatalf("estimate %d smaller than compressed %d", scan.TarSize, len(data))
	}
}

func TestRoundTripSingleFile(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "text1.txt"), "only file", 0o600)
	dest := t.TempDir()
	top, err := unpackZstd(t, packZstd(t, filepath.Join(src, "text1.txt"), "text1.txt"), dest)
	if err != nil || top != "text1.txt" {
		t.Fatalf("top = %q, %v", top, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "text1.txt")); string(got) != "only file" {
		t.Fatalf("content = %q", got)
	}
}

func TestRootSymlinkIsFollowed(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "real/a.txt"), "a", 0o644)
	if err := os.Symlink(filepath.Join(src, "real"), filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	top, err := unpackZstd(t, packZstd(t, filepath.Join(src, "alias"), "alias"), dest)
	if err != nil || top != "alias" {
		t.Fatalf("top = %q, %v", top, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "alias/a.txt")); string(got) != "a" {
		t.Fatalf("content = %q", got)
	}
}

// rawFrame stores data in a zstd frame of uncompressed blocks, like the web upload
// page does for data that does not compress (or when the browser lacks WebAssembly).
func rawFrame(data []byte) []byte {
	const block = 128 << 10
	out := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38}
	for i := 0; i == 0 || i < len(data); i += block {
		c := data[i:min(i+block, len(data))]
		v := len(c) << 3
		if i+block >= len(data) {
			v |= 1
		}
		out = append(out, byte(v), byte(v>>8), byte(v>>16))
		out = append(out, c...)
	}
	return out
}

// Web uploads are a run of independent frames, some compressed and some raw.
func TestConcatenatedFrames(t *testing.T) {
	src := t.TempDir()
	root := filepath.Join(src, "web")
	big := strings.Repeat("dtlk web upload\n", 40000)
	mustWrite(t, filepath.Join(root, "big.txt"), big, 0o644)
	mustWrite(t, filepath.Join(root, "sub/a.txt"), "a", 0o644)
	var tarBuf bytes.Buffer
	if err := writeTar(context.Background(), &tarBuf, root, "web", func(string) {}, nil); err != nil {
		t.Fatal(err)
	}
	raw := tarBuf.Bytes()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := len(raw)/3, 2*len(raw)/3
	data := enc.EncodeAll(raw[a:b], rawFrame(raw[:a]))
	data = append(data, rawFrame(raw[b:])...)

	dest := t.TempDir()
	top, err := unpackZstd(t, data, dest)
	if err != nil || top != "web" {
		t.Fatalf("top = %q, %v", top, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "web/big.txt")); string(got) != big {
		t.Fatalf("big.txt differs (%d bytes)", len(got))
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "web/sub/a.txt")); string(got) != "a" {
		t.Fatalf("a.txt = %q", got)
	}
}

func rawTar(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = 1
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestExtractRejectsUnsafeEntries(t *testing.T) {
	cases := map[string][]*tar.Header{
		"dotdot":        {{Name: "top/../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"absolute":      {{Name: "/etc/evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"two tops":      {{Name: "a/x", Typeflag: tar.TypeReg, Mode: 0o644}, {Name: "b/y", Typeflag: tar.TypeReg, Mode: 0o644}},
		"hardlink":      {{Name: "top/h", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}},
		"device":        {{Name: "top/dev", Typeflag: tar.TypeChar, Mode: 0o644}},
		"empty archive": {},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dest := t.TempDir()
			if _, err := unpackZstd(t, rawTar(t, entries...), dest); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestExtractNeverWritesThroughSymlink(t *testing.T) {
	outside := t.TempDir()
	for _, second := range []*tar.Header{
		{Name: "top/l/x", Typeflag: tar.TypeSymlink, Linkname: "y"},
		{Name: "top/l/x", Typeflag: tar.TypeReg, Mode: 0o644},
	} {
		data := rawTar(t, &tar.Header{Name: "top/l", Typeflag: tar.TypeSymlink, Linkname: outside}, second)
		if _, err := unpackZstd(t, data, t.TempDir()); err == nil {
			t.Fatal("expected error")
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("wrote outside: %v", entries)
		}
	}
}

func TestParseUpArgs(t *testing.T) {
	cfg := map[string]string{"hours": "48", "tag": "asdf"}
	ok := []struct {
		args []string
		cfg  map[string]string
		want upOpts
	}{
		{[]string{"dir"}, nil, upOpts{24, "", "dir"}},
		{[]string{"-h", "3", "dir"}, nil, upOpts{3, "", "dir"}},
		{[]string{"dir", "-h", "48"}, nil, upOpts{48, "", "dir"}},
		{[]string{"-h=168", "dir"}, nil, upOpts{168, "", "dir"}},
		{[]string{"-h", "1", "--", "-weird"}, nil, upOpts{1, "", "-weird"}},
		{[]string{"-t", "MyProj", "dir"}, nil, upOpts{24, "myproj", "dir"}},
		{[]string{"dir", "-t", "x"}, nil, upOpts{24, "x", "dir"}},
		{[]string{"-t=a_b-1", "-h", "3", "dir"}, nil, upOpts{3, "a_b-1", "dir"}},
		{[]string{"-t", strings.Repeat("a", 64), "dir"}, nil, upOpts{24, strings.Repeat("a", 64), "dir"}},
		{[]string{"-t", "", "dir"}, nil, upOpts{24, "", "dir"}},
		{[]string{"a=b"}, nil, upOpts{24, "", "a=b"}},
		// config defaults; explicit options win; -t "" drops the configured tag
		{[]string{"dir"}, cfg, upOpts{48, "asdf", "dir"}},
		{[]string{"dir"}, map[string]string{"tag": "MyProj"}, upOpts{24, "myproj", "dir"}},
		{[]string{"-h", "3", "-t", "other", "dir"}, cfg, upOpts{3, "other", "dir"}},
		{[]string{"dir", "-t", ""}, cfg, upOpts{48, "", "dir"}},
		{[]string{"-t=", "dir"}, cfg, upOpts{48, "", "dir"}},
		{[]string{"-h", "5", "-t", "x", "dir"}, map[string]string{"hours": "500", "tag": "a.b"}, upOpts{5, "x", "dir"}},
	}
	for _, c := range ok {
		got, err := parseUpArgs(c.args, c.cfg)
		if err != nil || got != c.want {
			t.Errorf("%v %v: got %+v %v", c.args, c.cfg, got, err)
		}
	}
	bad := [][]string{
		{"-h", "0", "dir"}, {"-h", "169", "dir"}, {"-h", "1.5", "dir"}, {"-h", "abc", "dir"}, {"-h", "", "dir"},
		{"-h"}, {"dir", "-h"}, {}, {"a", "b"}, {"-x", "dir"},
		{"-t"}, {"dir", "-t"}, {"-t", "a b", "dir"},
		{"-t", strings.Repeat("a", 65), "dir"}, {"-t", "한글", "dir"}, {"-t", "a.b", "dir"},
		// old option names are gone
		{"--h", "3", "dir"}, {"--h=3", "dir"}, {"-n", "x", "dir"}, {"-n=x", "dir"},
	}
	for _, args := range bad {
		if _, err := parseUpArgs(args, nil); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
	for _, c := range []map[string]string{{"hours": "500"}, {"hours": "x"}, {"tag": "a.b"}} {
		if _, err := parseUpArgs([]string{"dir"}, c); err == nil || !strings.Contains(err.Error(), configPath()) {
			t.Errorf("config %v: expected error naming the config file, got %v", c, err)
		}
	}
}

func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "text1 (1).txt"), "", 0o644)
	cases := []struct {
		name  string
		isDir bool
		want  string
	}{
		{"test1", true, "test1 (1)"},
		{"my.folder", true, "my.folder (1)"},
		{"text1.txt", false, "text1 (2).txt"},
		{"backup.tar.gz", false, "backup (1).tar.gz"},
		{".bashrc", false, ".bashrc (1)"},
	}
	for _, c := range cases {
		if got := freeName(dir, c.name, c.isDir); got != c.want {
			t.Errorf("freeName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "test1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(input string) (string, bool, error) {
		var out strings.Builder
		p := &prompter{in: bufioReader(input), out: &out}
		return resolveTarget(dir, "test1", true, p)
	}
	if got, ow, err := run("y\n"); err != nil || got != "test1" || !ow {
		t.Errorf("y: %q %v %v", got, ow, err)
	}
	if got, ow, err := run("n\n\n"); err != nil || got != "test1 (1)" || ow {
		t.Errorf("n+default: %q %v %v", got, ow, err)
	}
	if got, ow, err := run("n\nfresh\n"); err != nil || got != "fresh" || ow {
		t.Errorf("n+name: %q %v %v", got, ow, err)
	}
	// a typed name that also exists asks y/n again
	if got, ow, err := run("n\ntaken\nmaybe\ny\n"); err != nil || got != "taken" || !ow {
		t.Errorf("n+taken+y: %q %v %v", got, ow, err)
	}
	if _, _, err := run(""); err == nil {
		t.Error("EOF should fail")
	}
	if _, _, err := resolveTarget(dir, "test1", true, nil); err == nil {
		t.Error("non-interactive conflict should fail")
	}
	if got, ow, err := resolveTarget(dir, "new", true, nil); err != nil || got != "new" || ow {
		t.Errorf("no conflict: %q %v %v", got, ow, err)
	}
}
