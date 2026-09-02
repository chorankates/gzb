package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// What the prompt needs from a terminal that the line editor does not give
// it: keys that can be read by whichever of two parties wants them, a history
// that outlives the session, and a way to show a list of completions under
// the line being typed.

// keyPump owns standard input for the whole session, so that the line editor
// and a running command can take turns reading keys without contending for
// one descriptor. A read on a terminal cannot be cancelled, so whichever of
// the two wanted the next key would otherwise be stuck holding it.
type keyPump struct {
	keys chan byte
	done chan struct{} // closed when the input ends
}

func newKeyPump(r io.Reader) *keyPump {
	p := &keyPump{keys: make(chan byte, 4096), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		buf := make([]byte, 256)
		for {
			n, err := r.Read(buf)
			for _, b := range buf[:n] {
				p.keys <- b
			}
			if err != nil {
				return
			}
		}
	}()
	return p
}

// Read hands the line editor what has been typed: it waits for one key, then
// takes whatever else is already there, so an escape sequence arrives whole.
func (p *keyPump) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	select {
	case b[0] = <-p.keys:
	default:
		select {
		case b[0] = <-p.keys:
		case <-p.done:
			return 0, io.EOF
		}
	}
	n := 1
	for n < len(b) {
		select {
		case b[n] = <-p.keys:
			n++
		default:
			return n, nil
		}
	}
	return n, nil
}

// watch consumes keys until stop is called, passing each to on. It is how a
// command in progress hears Ctrl-C without the line editor being involved.
// Anything else typed while a command runs is discarded rather than queued
// for the next prompt: it was typed at something that was not listening.
func (p *keyPump) watch(on func(key byte)) (stop func()) {
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case k := <-p.keys:
				on(k)
			case <-p.done:
				return
			case <-quit:
				return
			}
		}
	}()
	return func() {
		close(quit)
		<-finished
	}
}

// terminalIO is what the line editor reads keys from and writes the line to.
type terminalIO struct {
	io.Reader
	io.Writer
}

// maxHistory bounds what is kept. Five hundred lines is more than a person
// scrolls back through and less than anyone notices on disk.
const maxHistory = 500

// fileHistory keeps the lines typed at the prompt across sessions, in a file
// beside the registry. A prompt whose history vanishes when it closes has
// forgotten the one thing a person most wants back: the command that worked.
type fileHistory struct {
	path  string
	lines []string
}

func historyPath(registry string) string {
	return filepath.Join(filepath.Dir(registry), "history")
}

// loadHistory reads what earlier sessions typed. A history that cannot be
// read is an empty one; nothing about a prompt should fail for want of it.
func loadHistory(path string) *fileHistory {
	h := &fileHistory{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return h
	}
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			h.lines = append(h.lines, line)
		}
	}
	if len(h.lines) > maxHistory {
		// The file only ever grows by appending, so this is where it is
		// trimmed: once, on the way in, rather than rewritten on every line.
		h.lines = h.lines[len(h.lines)-maxHistory:]
		os.WriteFile(path, []byte(strings.Join(h.lines, "\n")+"\n"), 0o600)
	}
	return h
}

// Add records a line, and writes it through rather than at exit, so a session
// that ends badly keeps what was typed. A line the same as the last is not
// worth a second entry, and an empty one is not worth any.
func (h *fileHistory) Add(entry string) {
	entry = strings.TrimSpace(entry)
	if entry == "" || (len(h.lines) > 0 && h.lines[len(h.lines)-1] == entry) {
		return
	}
	h.lines = append(h.lines, entry)
	if len(h.lines) > maxHistory {
		h.lines = h.lines[1:]
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, entry)
}

func (h *fileHistory) Len() int { return len(h.lines) }

// At returns an entry, most recent first, as the line editor asks for them.
func (h *fileHistory) At(i int) string { return h.lines[len(h.lines)-1-i] }

// onKey is the line editor's hook for every key. Only Tab is ours: complete
// the word at the cursor if there is one way to, take it as far as the
// candidates agree if there are several, and show them if that is nowhere.
func (s *session) onKey(line string, pos int, key rune) (string, int, bool) {
	if key != '\t' {
		return "", 0, false
	}
	c := grammar{commands: s.commands, devices: s.coordinator.Devices()}.complete(line[:pos])
	newLine, newPos, changed, show := completeAt(line, pos, c)
	if show {
		s.showCandidates(line, pos, c.candidates)
	}
	return newLine, newPos, changed
}

// completeAt works out what Tab does to a line: replaces the word at the
// cursor when one candidate is left, extends it as far as several agree, or
// — when that is no further than what is typed — asks for them to be shown.
func completeAt(line string, pos int, c completion) (newLine string, newPos int, changed, show bool) {
	if len(c.candidates) == 0 {
		return "", 0, false, false
	}
	typed := line[c.start:pos]
	tail := line[pos:]
	if len(c.candidates) == 1 {
		replacement := quoteWord(c.candidates[0]) + " "
		return line[:c.start] + replacement + tail, c.start + len(replacement), true, false
	}
	// Progress is measured in letters, not quote marks: an opening quote with
	// nothing after it is not a completion, it is a list waiting to be shown.
	prefix := commonPrefix(c.candidates)
	if len(prefix) > len(strings.ReplaceAll(typed, `"`, "")) {
		partial := quotePartial(prefix, c.candidates)
		return line[:c.start] + partial + tail, c.start + len(partial), true, false
	}
	return "", 0, false, true
}

// showCandidates prints the candidates under the line being edited and puts
// the line back, cursor and all.
//
// The line editor holds its lock while it asks about a key, so this cannot go
// through it; it goes straight to the terminal instead. That works only
// because the prompt and line are redrawn exactly as the editor drew them,
// with the cursor at the same place relative to where the prompt starts —
// which is the only thing the editor keeps track of.
func (s *session) showCandidates(line string, pos int, candidates []string) {
	width := s.width
	if width <= 0 {
		width = 80
	}
	promptLen := utf8.RuneCountInString(s.prompt)
	end := promptLen + utf8.RuneCountInString(line)
	cursor := promptLen + utf8.RuneCountInString(line[:pos])
	endRow, endCol := end/width, end%width
	cursorRow, cursorCol := cursor/width, cursor%width

	var b strings.Builder
	// Down to the last row of the line, so the list starts under all of it.
	if down := endRow - cursorRow; down > 0 {
		fmt.Fprintf(&b, "\x1b[%dB", down)
	}
	b.WriteString("\r\n")
	for _, row := range columns(candidates, width) {
		b.WriteString(row)
		b.WriteString("\r\n")
	}
	b.WriteString(s.prompt)
	b.WriteString(line)
	// A line that exactly fills its last row leaves the cursor at the start
	// of the next one, as the editor does.
	if end > 0 && endCol == 0 {
		b.WriteString("\r\n")
	}
	if up := endRow - cursorRow; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	switch shift := endCol - cursorCol; {
	case shift > 0:
		fmt.Fprintf(&b, "\x1b[%dD", shift)
	case shift < 0:
		fmt.Fprintf(&b, "\x1b[%dC", -shift)
	}
	os.Stdout.WriteString(b.String())
}

// columns lays items out in as many columns as fit, down then across, the
// way a directory listing does.
func columns(items []string, width int) []string {
	if len(items) == 0 {
		return nil
	}
	colWidth := 0
	for _, item := range items {
		colWidth = max(colWidth, utf8.RuneCountInString(item)+2)
	}
	cols := max(1, width/colWidth)
	rows := (len(items) + cols - 1) / cols
	lines := make([]string, rows)
	for i, item := range items {
		row, col := i%rows, i/rows
		if col == 0 {
			lines[row] = "  "
		}
		lines[row] += item
		// Pad only when something follows on the row, so no line ends in
		// spaces.
		if i+rows < len(items) {
			lines[row] += strings.Repeat(" ", colWidth-utf8.RuneCountInString(item))
		}
	}
	return lines
}
