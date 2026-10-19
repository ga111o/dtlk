package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeClipboard puts fake tools on an otherwise empty PATH. A tool named in ok
// records "<args>|<stdin>" in <name>.out and succeeds; the others fail.
// The OSC 52 output goes to the returned file.
func fakeClipboard(t *testing.T, env map[string]string, ok ...string) (bin, tty string) {
	t.Helper()
	bin = t.TempDir()
	for _, name := range []string{"wl-copy", "xclip", "xsel", "clip.exe", "tmux"} {
		script := "#!/bin/sh\nexit 1\n"
		for _, o := range ok {
			if o == name {
				script = "#!/bin/sh\nIFS= read -r x || :\nprintf '%s|%s' \"$*\" \"$x\" > \"$0.out\"\n"
			}
		}
		mustWrite(t, filepath.Join(bin, name), script, 0o755)
	}
	t.Setenv("PATH", bin)
	for _, k := range []string{"WAYLAND_DISPLAY", "DISPLAY", "TMUX", "STY", "TERM"} {
		t.Setenv(k, env[k])
	}
	tty = filepath.Join(t.TempDir(), "tty")
	mustWrite(t, tty, "", 0o600)
	old := ttyPath
	ttyPath = tty
	t.Cleanup(func() { ttyPath = old })
	return bin, tty
}

func toolOutput(t *testing.T, bin, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(bin, name+".out"))
	if os.IsNotExist(err) {
		return "(not run)"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCopyToClipboard(t *testing.T) {
	const url = "https://dtlk.ga111o.com/482913"
	osc := "\x1b]52;c;aHR0cHM6Ly9kdGxrLmdhMTExby5jb20vNDgyOTEz\a"
	cases := []struct {
		name  string
		env   map[string]string
		ok    []string
		tools map[string]string // expected tool output
		tty   string
	}{
		{
			name:  "wayland",
			env:   map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"},
			ok:    []string{"wl-copy", "xclip"},
			tools: map[string]string{"wl-copy": "|" + url, "xclip": "(not run)"},
			tty:   osc,
		},
		{
			name:  "x11 falls back to xsel",
			env:   map[string]string{"DISPLAY": ":0"},
			ok:    []string{"xsel", "wl-copy"},
			tools: map[string]string{"xsel": "--clipboard --input|" + url, "wl-copy": "(not run)"},
			tty:   osc,
		},
		{
			name:  "no display: only clip.exe",
			env:   map[string]string{},
			ok:    []string{"wl-copy", "xclip", "clip.exe"},
			tools: map[string]string{"clip.exe": "|" + url, "wl-copy": "(not run)", "xclip": "(not run)"},
			tty:   osc,
		},
		{
			name:  "tmux forwards OSC 52",
			env:   map[string]string{"TMUX": "/tmp/tmux-1000/default,1,0"},
			ok:    []string{"tmux"},
			tools: map[string]string{"tmux": "load-buffer -w -|" + url},
			tty:   "",
		},
		{
			name: "old tmux: plain OSC 52",
			env:  map[string]string{"TMUX": "/tmp/tmux-1000/default,1,0"},
			tty:  osc,
		},
		{
			name: "screen",
			env:  map[string]string{"STY": "1234.pts-0.host"},
			tty:  "\x1bP" + osc + "\x1b\\",
		},
		{
			name: "dumb terminal",
			env:  map[string]string{"TERM": "dumb"},
			tty:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, tty := fakeClipboard(t, tc.env, tc.ok...)
			copyToClipboard(url)
			for name, want := range tc.tools {
				if got := toolOutput(t, bin, name); got != want {
					t.Errorf("%s: got %q, want %q", name, got, want)
				}
			}
			if got := readFile(t, tty); got != tc.tty {
				t.Errorf("tty: got %q, want %q", got, tc.tty)
			}
		})
	}
}
