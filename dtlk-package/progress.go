package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), -1
	for v >= unit && i < 3 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %cB", v, "KMGT"[i])
}

// progress redraws one status line on stderr while a transfer runs.
// It stays silent when stderr is not a terminal.
type progress struct {
	label string
	total int64
	done  atomic.Int64
	start time.Time
	stop  chan struct{}
	wg    sync.WaitGroup
	mu    sync.Mutex
	tty   bool
}

func startProgress(label string, total int64) *progress {
	p := &progress{label: label, total: total, start: time.Now(), stop: make(chan struct{}), tty: isTerminal(os.Stderr)}
	if p.tty {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			t := time.NewTicker(200 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-p.stop:
					return
				case <-t.C:
					p.render()
				}
			}
		}()
	}
	return p
}

func (p *progress) add(n int) { p.done.Add(int64(n)) }

func (p *progress) render() {
	p.mu.Lock()
	defer p.mu.Unlock()
	done := p.done.Load()
	elapsed := time.Since(p.start).Seconds()
	speed := ""
	if elapsed > 0.5 {
		speed = humanBytes(int64(float64(done)/elapsed)) + "/s"
	}
	if p.total > 0 {
		pct := min(float64(done)*100/float64(p.total), 100)
		fmt.Fprintf(os.Stderr, "\r\033[K%s %5.1f%%  %s / %s  %s", p.label, pct, humanBytes(done), humanBytes(p.total), speed)
	} else {
		fmt.Fprintf(os.Stderr, "\r\033[K%s %s  %s", p.label, humanBytes(done), speed)
	}
}

// note prints a message above the progress line.
func (p *progress) note(msg string) {
	p.mu.Lock()
	if p.tty {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	fmt.Fprintln(os.Stderr, msg)
	p.mu.Unlock()
}

func (p *progress) finish() {
	if !p.tty {
		return
	}
	select {
	case <-p.stop:
		return
	default:
	}
	close(p.stop)
	p.wg.Wait()
	p.render()
	fmt.Fprintln(os.Stderr)
}
