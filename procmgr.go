//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	ps "github.com/mitchellh/go-ps"
	"github.com/olekukonko/ts"
	"golang.org/x/sys/unix"
)

// ── ANSI escape codes ─────────────────────────────────────────────────────────

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRev    = "\x1b[7m"
	fgRed      = "\x1b[31m"
	fgGreen    = "\x1b[32m"
	fgYellow   = "\x1b[33m"
	fgCyan     = "\x1b[36m"
	bgDarkBlue = "\x1b[48;5;17m"
	bgDarkGreen = "\x1b[48;5;22m"
)

// ── Data model ────────────────────────────────────────────────────────────────

type process struct {
	name  string
	pid   int
	memKB int64
}

type uiMode int

const (
	modeNormal  uiMode = iota
	modeSearch
	modeConfirm
)

type sortField int

const (
	sortByMem  sortField = iota
	sortByName
)

// ── TUI state ─────────────────────────────────────────────────────────────────

type tui struct {
	procs    []process
	filtered []int        // indices into procs
	cursor   int          // index within filtered
	offset   int          // scroll offset within filtered
	selected map[int]bool // pid → selected

	search   string
	mode     uiMode
	message  string
	sortBy   sortField
	sortDesc bool

	width  int
	height int

	orig unix.Termios
	in   *bufio.Reader
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	t := &tui{
		selected: make(map[int]bool),
		in:       bufio.NewReaderSize(os.Stdin, 64),
		sortBy:   sortByMem,
		sortDesc: true,
	}

	if err := t.initTerm(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer t.cleanupTerm()

	if err := t.reload(); err != nil {
		t.cleanupTerm()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for {
		t.draw()
		if t.handleInput() {
			return
		}
	}
}

// ── Terminal management ───────────────────────────────────────────────────────

func (t *tui) initTerm() error {
	orig, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TIOCGETA)
	if err != nil {
		return fmt.Errorf("get termios: %w", err)
	}
	t.orig = *orig

	raw := *orig
	// Disable echo, canonical mode, signals, and extended processing.
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	// Disable flow control and input character translation.
	raw.Iflag &^= unix.IXON | unix.ICRNL | unix.BRKINT | unix.INPCK | unix.ISTRIP
	// 8-bit chars.
	raw.Cflag |= unix.CS8
	// Read one byte at a time, no timeout.
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, &raw); err != nil {
		return fmt.Errorf("set termios: %w", err)
	}
	fmt.Print("\x1b[?25l") // hide cursor
	return nil
}

func (t *tui) cleanupTerm() {
	fmt.Print("\x1b[?25h\x1b[2J\x1b[H") // show cursor + clear screen
	unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, &t.orig)
}

// ── Data loading ──────────────────────────────────────────────────────────────

func (t *tui) reload() error {
	psList, err := ps.Processes()
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}

	mem := fetchMem()

	seen := make(map[int]bool)
	procs := make([]process, 0, len(psList))
	for _, p := range psList {
		if seen[p.Pid()] {
			continue
		}
		seen[p.Pid()] = true
		procs = append(procs, process{
			name:  fullName(p.Pid(), p.Executable()),
			pid:   p.Pid(),
			memKB: mem[p.Pid()],
		})
	}

	t.procs = procs
	t.sortProcs()
	t.refilter()
	t.message = fmt.Sprintf("Loaded %d processes", len(procs))
	return nil
}

// sortProcs sorts t.procs according to t.sortBy and t.sortDesc.
func (t *tui) sortProcs() {
	sort.SliceStable(t.procs, func(i, j int) bool {
		pi, pj := t.procs[i], t.procs[j]
		var less bool
		switch t.sortBy {
		case sortByName:
			ni, nj := strings.ToLower(pi.name), strings.ToLower(pj.name)
			if ni != nj {
				less = ni < nj
			} else {
				less = pi.pid < pj.pid
			}
		default: // sortByMem
			if pi.memKB != pj.memKB {
				less = pi.memKB < pj.memKB
			} else {
				less = strings.ToLower(pi.name) < strings.ToLower(pj.name)
			}
		}
		if t.sortDesc {
			return !less
		}
		return less
	})
}

// fetchMem returns a pid→RSS(KB) map using the system `ps` command.
func fetchMem() map[int]int64 {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,rss=").Output()
	if err != nil {
		return nil
	}
	m := make(map[int]int64)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		rss, e2 := strconv.ParseInt(f[1], 10, 64)
		if e1 == nil && e2 == nil {
			m[pid] = rss
		}
	}
	return m
}

// fullName returns the untruncated executable name via kern.procargs2 on macOS,
// falling back to the 16-char name returned by go-ps.
func fullName(pid int, fallback string) string {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(buf) < 4 {
		return fallback
	}
	rest := buf[4:]
	if i := bytes.IndexByte(rest, 0); i > 0 {
		path := string(rest[:i])
		if j := strings.LastIndexByte(path, '/'); j >= 0 {
			return path[j+1:]
		}
		return path
	}
	return fallback
}

// refilter rebuilds t.filtered from the current search string and clamps cursor.
func (t *tui) refilter() {
	q := strings.ToLower(t.search)
	t.filtered = t.filtered[:0]
	for i, p := range t.procs {
		if q == "" ||
			strings.Contains(strings.ToLower(p.name), q) ||
			strings.Contains(strconv.Itoa(p.pid), q) {
			t.filtered = append(t.filtered, i)
		}
	}
	if t.cursor >= len(t.filtered) {
		t.cursor = max(0, len(t.filtered)-1)
	}
}

// ── Layout constants ──────────────────────────────────────────────────────────

const (
	// Fixed column widths (including trailing padding/space):
	//   marker:  2  ("► " or "  ")
	//   check:   4  ("[ ] " or "[*] ")
	//   gap:     2  between name and PID
	//   pid:     7  right-aligned
	//   gap:     2  between PID and memory
	//   mem:     9  right-aligned
	//   total fixed = 2+4+2+7+2+9 = 26
	fixedCols = 26
	pidCols   = 7
	memCols   = 9
)

func (t *tui) updateSize() {
	t.width, t.height = 80, 24
	if sz, err := ts.GetSize(); err == nil && sz.Col() > 0 {
		t.width = sz.Col()
		t.height = sz.Row()
	}
}

func (t *tui) nameWidth() int {
	n := t.width - fixedCols
	if n < 20 {
		return 20
	}
	return n
}

// listRows returns the number of visible process rows.
// Layout: title(1) + colheader(1) + sep(1) + list(n) + sep(1) + statusbar(1) = height
func (t *tui) listRows() int {
	h := t.height - 5
	if h < 1 {
		return 1
	}
	return h
}

// ── Rendering ─────────────────────────────────────────────────────────────────

func (t *tui) draw() {
	t.updateSize()
	nw := t.nameWidth()
	lh := t.listRows()

	// Keep cursor visible within the scroll window.
	if t.cursor < t.offset {
		t.offset = t.cursor
	}
	if t.cursor >= t.offset+lh {
		t.offset = t.cursor - lh + 1
	}

	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H") // clear screen and home cursor

	// ── Title bar ─────────────────────────────────────────────────────────
	selCount := len(t.selected)
	info := fmt.Sprintf(" procmgr  %d processes", len(t.procs))
	if t.search != "" {
		info += fmt.Sprintf("  filter:\"%s\" (%d)", t.search, len(t.filtered))
	}
	if selCount > 0 {
		info += fmt.Sprintf("  [%d selected]", selCount)
	}
	b.WriteString(ansiRev + ansiBold + rpad(info, t.width) + ansiReset + "\r\n")

	// ── Column header ──────────────────────────────────────────────────────
	sortArrow := func(field sortField) string {
		if t.sortBy != field {
			return ""
		}
		if t.sortDesc {
			return " v"
		}
		return " ^"
	}
	header := fmt.Sprintf("      %-*s  %s  %s",
		nw, "NAME"+sortArrow(sortByName),
		lpad("PID", pidCols),
		lpad("MEMORY"+sortArrow(sortByMem), memCols),
	)
	b.WriteString(ansiBold + ansiDim + rpad(header, t.width) + ansiReset + "\r\n")

	// ── Top separator ─────────────────────────────────────────────────────
	b.WriteString(ansiDim + strings.Repeat("─", t.width) + ansiReset + "\r\n")

	// ── Process rows ──────────────────────────────────────────────────────
	for row := range lh {
		idx := t.offset + row
		if idx >= len(t.filtered) {
			b.WriteString("\r\n")
			continue
		}

		pi := t.filtered[idx]
		p := t.procs[pi]
		isCursor := idx == t.cursor
		isSel := t.selected[p.pid]

		check := "[ ]"
		if isSel {
			check = "[*]"
		}
		marker := "  "
		if isCursor {
			marker = "► "
		}

		name := trunc(p.name, nw)
		pidStr := lpad(strconv.Itoa(p.pid), pidCols)
		memStr := lpad(fmtMem(p.memKB), memCols)
		content := fmt.Sprintf("%s%s %-*s  %s  %s",
			marker, check, nw, name, pidStr, memStr)
		line := rpad(content, t.width)

		switch {
		case isCursor && isSel:
			b.WriteString(bgDarkGreen + fgYellow + ansiBold + line + ansiReset)
		case isCursor:
			b.WriteString(bgDarkBlue + fgCyan + line + ansiReset)
		case isSel:
			b.WriteString(fgGreen + line + ansiReset)
		default:
			b.WriteString(line)
		}
		b.WriteString("\r\n")
	}

	// ── Bottom separator ──────────────────────────────────────────────────
	b.WriteString(ansiDim + strings.Repeat("─", t.width) + ansiReset + "\r\n")

	// ── Status / help bar ─────────────────────────────────────────────────
	switch t.mode {
	case modeSearch:
		b.WriteString(fgCyan + "Search: " + ansiReset + t.search + "█")
	case modeConfirm:
		b.WriteString(fgRed + ansiBold +
			fmt.Sprintf("Kill %d process(es)? [y/N]: ", selCount) +
			ansiReset)
	default:
		if t.message != "" {
			b.WriteString(fgGreen + t.message + ansiReset)
		} else {
			b.WriteString(ansiDim +
				"↑↓: move  spc: select  a: all  A: none  k: kill  /: search  n: sort name  m: sort mem  r: reload  q: quit" +
				ansiReset)
		}
	}

	fmt.Print(b.String())
}

// ── Input handling ────────────────────────────────────────────────────────────

// handleInput reads one keypress and dispatches it. Returns true to quit.
func (t *tui) handleInput() (quit bool) {
	key := t.readKey()
	t.message = "" // clear transient message on any keypress

	switch t.mode {
	case modeSearch:
		return t.handleSearchKey(key)
	case modeConfirm:
		return t.handleConfirmKey(key)
	default:
		return t.handleNormalKey(key)
	}
}

func (t *tui) handleNormalKey(key string) bool {
	switch key {
	case "q", "ctrl+c", "ctrl+d":
		return true

	case "up":
		if t.cursor > 0 {
			t.cursor--
		}
	case "down":
		if t.cursor < len(t.filtered)-1 {
			t.cursor++
		}

	case " ":
		t.toggleCursor()

	case "a":
		for _, i := range t.filtered {
			t.selected[t.procs[i].pid] = true
		}
		t.message = fmt.Sprintf("Selected all %d visible processes", len(t.filtered))

	case "A", "esc":
		t.selected = make(map[int]bool)
		t.search = ""
		t.refilter()
		t.message = "Selection and filter cleared"

	case "k":
		if len(t.selected) == 0 {
			t.message = "Nothing selected — use Space to select processes"
		} else {
			t.mode = modeConfirm
		}

	case "/":
		t.mode = modeSearch

	case "n":
		if t.sortBy == sortByName {
			t.sortDesc = !t.sortDesc
		} else {
			t.sortBy = sortByName
			t.sortDesc = false
		}
		t.sortProcs()
		t.refilter()
		if t.sortDesc {
			t.message = "Sorted by name Z→A"
		} else {
			t.message = "Sorted by name A→Z"
		}

	case "m":
		if t.sortBy == sortByMem {
			t.sortDesc = !t.sortDesc
		} else {
			t.sortBy = sortByMem
			t.sortDesc = true
		}
		t.sortProcs()
		t.refilter()
		if t.sortDesc {
			t.message = "Sorted by memory high→low"
		} else {
			t.message = "Sorted by memory low→high"
		}

	case "r":
		if err := t.reload(); err != nil {
			t.message = "Reload error: " + err.Error()
		}
	}
	return false
}

func (t *tui) handleSearchKey(key string) bool {
	switch key {
	case "ctrl+c", "ctrl+d":
		return true
	case "esc", "enter":
		t.mode = modeNormal
	case "backspace":
		if len(t.search) > 0 {
			t.search = t.search[:len(t.search)-1]
			t.refilter()
		}
	default:
		if len(key) == 1 && key[0] >= 32 {
			t.search += key
			t.refilter()
		}
	}
	return false
}

func (t *tui) handleConfirmKey(key string) bool {
	switch key {
	case "y", "Y":
		t.mode = modeNormal
		killed, failed := 0, 0
		for pid := range t.selected {
			if killPID(pid) == nil {
				killed++
			} else {
				failed++
			}
		}
		t.selected = make(map[int]bool)
		t.reload()
		t.message = fmt.Sprintf("Killed %d process(es)", killed)
		if failed > 0 {
			t.message += fmt.Sprintf(", %d failed (permission denied?)", failed)
		}
	case "n", "N", "esc", "enter", "q":
		t.mode = modeNormal
		t.message = "Kill cancelled"
	case "ctrl+c", "ctrl+d":
		return true
	}
	return false
}

func (t *tui) toggleCursor() {
	if len(t.filtered) == 0 {
		return
	}
	pid := t.procs[t.filtered[t.cursor]].pid
	if t.selected[pid] {
		delete(t.selected, pid)
	} else {
		t.selected[pid] = true
	}
}

func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// ── Key reading ───────────────────────────────────────────────────────────────

func (t *tui) readKey() string {
	b, err := t.in.ReadByte()
	if err != nil {
		return "ctrl+d"
	}
	switch b {
	case 3:
		return "ctrl+c"
	case 4:
		return "ctrl+d"
	case 27: // ESC or ANSI escape sequence
		seq := t.readEscSeq()
		switch seq {
		case "[A":
			return "up"
		case "[B":
			return "down"
		case "[C":
			return "right"
		case "[D":
			return "left"
		case "[3~":
			return "del"
		}
		return "esc"
	case 127:
		return "backspace"
	case 13, 10:
		return "enter"
	}
	return string([]byte{b})
}

// readEscSeq reads the bytes following ESC. It temporarily switches the
// terminal to VMIN=0, VTIME=1 (100 ms timeout) so that bare ESC and
// multi-byte escape sequences are both handled correctly.
func (t *tui) readEscSeq() string {
	cur, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TIOCGETA)
	if err != nil {
		return ""
	}
	// Short timeout: return immediately if no byte arrives within 100 ms.
	tmp := *cur
	tmp.Cc[unix.VMIN] = 0
	tmp.Cc[unix.VTIME] = 1
	unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, &tmp)
	defer func() {
		cur.Cc[unix.VMIN] = 1
		cur.Cc[unix.VTIME] = 0
		unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, cur)
	}()

	var seq []byte
	for range 5 {
		b, err := t.in.ReadByte()
		if err != nil {
			break
		}
		seq = append(seq, b)
		// A sequence ending in a letter or '~' is complete.
		last := seq[len(seq)-1]
		if seq[0] == '[' && len(seq) >= 2 {
			if (last >= 'A' && last <= 'Z') || (last >= 'a' && last <= 'z') || last == '~' {
				break
			}
		}
	}
	return string(seq)
}

// ── String helpers ────────────────────────────────────────────────────────────

// rpad right-pads (or truncates) s to exactly n bytes.
func rpad(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}

// lpad left-pads s to at least n bytes.
func lpad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat(" ", n-len(s)) + s
}

// trunc truncates s to n bytes, adding "…" if it was longer.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// fmtMem formats a kilobyte value as a human-readable memory string.
func fmtMem(kb int64) string {
	if kb == 0 {
		return "-"
	}
	if kb < 1024 {
		return fmt.Sprintf("%d KB", kb)
	}
	mb := float64(kb) / 1024.0
	if mb < 1024 {
		return fmt.Sprintf("%.1f MB", mb)
	}
	return fmt.Sprintf("%.2f GB", mb/1024.0)
}

