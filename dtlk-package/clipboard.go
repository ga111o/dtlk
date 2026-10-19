package main

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ttyPath is where the OSC 52 sequence goes (tests point it at a file).
var ttyPath = "/dev/tty"

// copyToClipboard puts text on the clipboard, silently and best effort. It uses
// the first local clipboard tool that works in this session, and always also
// sends OSC 52 to the terminal, which reaches the clipboard of the machine the
// user sits at even over SSH.
func copyToClipboard(text string) {
	for _, argv := range clipboardCommands() {
		if runClipboard(argv, text) == nil {
			break
		}
	}
	copyOSC52(text)
}

// clipboardCommands lists the local clipboard tools to try, in order, judging
// by which display servers this session can reach.
func clipboardCommands() [][]string {
	var cmds [][]string
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		cmds = append(cmds, []string{"wl-copy"})
	}
	if os.Getenv("DISPLAY") != "" {
		cmds = append(cmds, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
	}
	return append(cmds, []string{"clip.exe"}) // WSL: the Windows clipboard
}

func runClipboard(argv []string, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(text)
	// wl-copy and xclip leave a child behind that serves the clipboard; a new
	// session keeps it alive after the terminal closes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Run()
}

// copyOSC52 asks the terminal to set its clipboard. Terminals without OSC 52
// support ignore the sequence.
func copyOSC52(text string) {
	if os.Getenv("TERM") == "dumb" {
		return
	}
	// tmux drops OSC 52 from programs by default; load-buffer -w (tmux 3.2+)
	// forwards it to the outer terminal.
	if os.Getenv("TMUX") != "" && runClipboard([]string{"tmux", "load-buffer", "-w", "-"}, text) == nil {
		return
	}
	tty, err := os.OpenFile(ttyPath, os.O_WRONLY, 0)
	if err != nil {
		return // no controlling terminal
	}
	defer tty.Close()
	tty.WriteString(osc52(text, os.Getenv("STY") != ""))
}

func osc52(text string, screen bool) string {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	if screen {
		return "\x1bP" + seq + "\x1b\\" // GNU screen passes DCS contents to the outer terminal
	}
	return seq
}
