// Package progress draws the one self-updating stderr line that stops the CLI looking hung
// (cli.mdx §13). It animates only when stderr is a TTY, never touches stdout, and erases itself
// fully before anything else prints.
package progress

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner is one progress line
type Spinner struct {
	mu      sync.Mutex
	phase   string
	count   string
	started time.Time
	stop    chan struct{}
	done    chan struct{}
	tty     bool
	quiet   bool
	active  bool
	lastLen int
}

// IsTTY reports whether stderr is a terminal
func IsTTY() bool {
	fi, err := os.Stderr.Stat()

	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// New returns a spinner; quiet suppresses it entirely
func New(quiet bool) *Spinner {
	return &Spinner{tty: IsTTY(), quiet: quiet}
}

// Start begins drawing with a phase text. Non-TTY stderr gets one plain status line instead.
func (s *Spinner) Start(phase string) {
	if s.quiet {
		return
	}

	s.mu.Lock()
	s.phase = phase
	s.started = time.Now()

	if !s.tty {
		s.mu.Unlock()
		fmt.Fprintln(os.Stderr, phase)
		return
	}

	if s.active {
		s.mu.Unlock()
		return
	}

	s.active = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.mu.Unlock()

	go s.loop()
}

// Tick updates the phase text and an optional "n/total" count
func (s *Spinner) Tick(phase string, count string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if phase != "" {
		s.phase = phase
	}

	s.count = count
}

func (s *Spinner) loop() {
	defer close(s.done)

	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()

	i := 0

	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			elapsed := time.Since(s.started).Round(time.Second)
			line := fmt.Sprintf("%s %s %s  %s", frames[i%len(frames)], s.phase, s.count, fmtElapsed(elapsed))
			pad := ""

			if s.lastLen > len(line) {
				pad = strings.Repeat(" ", s.lastLen-len(line))
			}

			fmt.Fprint(os.Stderr, "\r"+line+pad)
			s.lastLen = len(line)
			s.mu.Unlock()
			i++
		}
	}
}

func fmtElapsed(d time.Duration) string {
	m := int(d.Minutes())
	sec := int(d.Seconds()) % 60

	return fmt.Sprintf("%dm%02ds", m, sec)
}

// Stop erases the line completely (the cleanup contract)
func (s *Spinner) Stop() {
	s.mu.Lock()

	if !s.active {
		s.mu.Unlock()
		return
	}

	s.active = false
	close(s.stop)
	s.mu.Unlock()
	<-s.done

	s.mu.Lock()
	fmt.Fprint(os.Stderr, "\r"+strings.Repeat(" ", s.lastLen+2)+"\r")
	s.lastLen = 0
	s.mu.Unlock()
}
