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
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// ── ANSI escape codes ─────────────────────────────────────────────────────────

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiRev     = "\x1b[7m"
	fgRed       = "\x1b[91m"
	fgGreen     = "\x1b[92m"
	fgYellow    = "\x1b[93m"
	fgBlue      = "\x1b[94m"
	fgMagenta   = "\x1b[95m"
	fgCyan      = "\x1b[96m"
	bgDarkBlue  = "\x1b[48;5;17m"
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
	modeNormal uiMode = iota
	modeSearch
	modeConfirm
)

type sortField int

const (
	sortByMem sortField = iota
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
	showAll  bool // true = all users, false = current user only

	width  int
	height int

	orig unix.Termios
	in   *bufio.Reader
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	tui := &tui{
		selected: make(map[int]bool),
		in:       bufio.NewReaderSize(os.Stdin, 64),
		sortBy:   sortByMem,
		sortDesc: true,
		showAll:  false,
	}

	if err := tui.initTerm(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer tui.cleanupTerm()

	if err := tui.reload(); err != nil {
		tui.cleanupTerm()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for {
		tui.draw()
		if tui.handleInput() {
			return
		}
	}
}

// ── Terminal management ───────────────────────────────────────────────────────

func (tui *tui) initTerm() error {
	orig, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TIOCGETA)
	if err != nil {
		return fmt.Errorf("get termios: %w", err)
	}
	tui.orig = *orig

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

func (tui *tui) cleanupTerm() {
	fmt.Print("\x1b[?25h\x1b[2J\x1b[H") // show cursor + clear screen
	unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, &tui.orig)
}

// ── Data loading ──────────────────────────────────────────────────────────────

func (tui *tui) reload() error {
	procs, err := listProcs(tui.showAll)
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	tui.procs = procs
	tui.sortProcs()
	tui.refilter()
	tui.message = fmt.Sprintf("Loaded %d processes", len(procs))
	return nil
}

// listProcs lists all running processes with their PID, name, and RSS memory
// using a single invocation of the system ps(1) command.
func listProcs(showAll bool) ([]process, error) {
	psArgs := "-x"
	if showAll {
		psArgs = "-ax"
	}
	out, err := exec.Command("ps", psArgs, "-o", "pid=,rss=,comm=").Output()
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool)
	var procs []process
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || seen[pid] {
			continue
		}
		seen[pid] = true
		rss, _ := strconv.ParseInt(f[1], 10, 64)
		comm := strings.Join(f[2:], " ")
		procs = append(procs, process{
			name:  fullName(pid, comm),
			pid:   pid,
			memKB: rss,
		})
	}
	return procs, nil
}

// sortProcs sorts tui.procs according to tui.sortBy and tui.sortDesc.
func (tui *tui) sortProcs() {
	sort.SliceStable(tui.procs, func(i, j int) bool {
		pi, pj := tui.procs[i], tui.procs[j]
		var less bool
		switch tui.sortBy {
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
		if tui.sortDesc {
			return !less
		}
		return less
	})
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

// refilter rebuilds tui.filtered from the current search string and clamps cursor.
func (tui *tui) refilter() {
	q := strings.ToLower(tui.search)
	tui.filtered = tui.filtered[:0]
	for i, p := range tui.procs {
		if q == "" ||
			strings.Contains(strings.ToLower(p.name), q) ||
			strings.Contains(strconv.Itoa(p.pid), q) {
			tui.filtered = append(tui.filtered, i)
		}
	}
	if tui.cursor >= len(tui.filtered) {
		tui.cursor = max(0, len(tui.filtered)-1)
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

func (tui *tui) updateSize() {
	tui.width, tui.height = 80, 24
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
		tui.width = int(ws.Col)
		tui.height = int(ws.Row)
	}
}

func (tui *tui) nameWidth() int {
	n := tui.width - fixedCols
	if n < 20 {
		return 20
	}
	return n
}

// statusBarText returns the plain (no ANSI) text for the status bar.
func (tui *tui) statusBarText(selCount int) string {
	switch tui.mode {
	case modeSearch:
		return "Search: " + tui.search + "█"
	case modeConfirm:
		return fmt.Sprintf("Kill %d process(es)? [y/N]: ", selCount)
	default:
		if tui.message != "" {
			return tui.message
		}
		return "↑↓: move | spc: select | a: all | A: none | k: kill | /: search | n: sort name | m: sort mem | u: user/all | r: reload | q: quit"
	}
}

// ── Rendering ─────────────────────────────────────────────────────────────────

func (tui *tui) draw() {
	tui.updateSize()
	nw := tui.nameWidth()

	// Compute status bar height first so the list height accounts for wrapping.
	selCount := len(tui.selected)
	statusTxt := tui.statusBarText(selCount)
	statusLines := max(1, (utf8.RuneCountInString(statusTxt)+tui.width-1)/tui.width)
	// Layout: title(1) + colheader(1) + sep(1) + list(lh) + sep(1) + status(statusLines) = height
	lh := max(1, tui.height-4-statusLines)

	// Keep cursor visible within the scroll window.
	if tui.cursor < tui.offset {
		tui.offset = tui.cursor
	}
	if tui.cursor >= tui.offset+lh {
		tui.offset = tui.cursor - lh + 1
	}

	var stringBuilder strings.Builder
	stringBuilder.WriteString("\x1b[2J\x1b[H") // clear screen and home cursor

	// ── Title bar ─────────────────────────────────────────────────────────
	scope := "all"
	if !tui.showAll {
		scope = "user"
	}
	info := fmt.Sprintf("procmgr | %d processes (%s)", len(tui.procs), scope)
	if tui.search != "" {
		info += fmt.Sprintf(" | filter:\"%s\" (%d)", tui.search, len(tui.filtered))
	}
	if selCount > 0 {
		info += fmt.Sprintf(" | %d selected", selCount)
	}
	stringBuilder.WriteString(ansiRev + ansiBold + rpad(info, tui.width) + ansiReset + "\r\n")

	// ── Column header ──────────────────────────────────────────────────────
	sortArrow := func(field sortField) string {
		if tui.sortBy != field {
			return ""
		}
		if tui.sortDesc {
			return " ↓"
		}
		return " ↑"
	}
	header := fmt.Sprintf("      %-*s  %s  %s",
		nw, "NAME"+sortArrow(sortByName), lpad("PID", pidCols), lpad("MEMORY"+sortArrow(sortByMem), memCols),
	)
	stringBuilder.WriteString(ansiBold + ansiDim + rpad(header, tui.width) + ansiReset + "\r\n")

	// ── Top separator ─────────────────────────────────────────────────────
	stringBuilder.WriteString(ansiDim + strings.Repeat("─", tui.width) + ansiReset + "\r\n")

	// ── Process rows ──────────────────────────────────────────────────────
	for row := range lh {
		idx := tui.offset + row
		if idx >= len(tui.filtered) {
			stringBuilder.WriteString("\r\n")
			continue
		}

		pi := tui.filtered[idx]
		p := tui.procs[pi]
		isCursor := idx == tui.cursor
		isSel := tui.selected[p.pid]

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
		content := fmt.Sprintf("%s%s %-*s  %s  %s", marker, check, nw, name, pidStr, memStr)
		line := rpad(content, tui.width)

		switch {
		case isCursor && isSel:
			stringBuilder.WriteString(bgDarkGreen + fgYellow + ansiBold + line + ansiReset)
		case isCursor:
			stringBuilder.WriteString(bgDarkBlue + fgCyan + line + ansiReset)
		case isSel:
			stringBuilder.WriteString(fgGreen + line + ansiReset)
		default:
			stringBuilder.WriteString(line)
		}
		stringBuilder.WriteString("\r\n")
	}

	// ── Bottom separator ──────────────────────────────────────────────────
	stringBuilder.WriteString(ansiDim + strings.Repeat("─", tui.width) + ansiReset + "\r\n")

	// ── Status / help bar ─────────────────────────────────────────────────
	switch tui.mode {
	case modeSearch:
		stringBuilder.WriteString(fgCyan + "Search: " + ansiReset + tui.search + "█")
	case modeConfirm:
		stringBuilder.WriteString(fgRed + ansiBold + statusTxt + ansiReset)
	default:
		if tui.message != "" {
			stringBuilder.WriteString(fgGreen + statusTxt + ansiReset)
		} else {
			stringBuilder.WriteString(fgCyan + statusTxt + ansiReset)
		}
	}

	fmt.Print(stringBuilder.String())
}

// ── Input handling ────────────────────────────────────────────────────────────

// handleInput reads one keypress and dispatches it. Returns true to quit.
func (tui *tui) handleInput() (quit bool) {
	key := tui.readKey()
	tui.message = "" // clear transient message on any keypress

	switch tui.mode {
	case modeSearch:
		return tui.handleSearchKey(key)
	case modeConfirm:
		return tui.handleConfirmKey(key)
	default:
		return tui.handleNormalKey(key)
	}
}

func (tui *tui) handleNormalKey(key string) bool {
	switch key {
	case "q", "ctrl+c", "ctrl+d":
		return true

	case "up":
		if tui.cursor > 0 {
			tui.cursor--
		}
	case "down":
		if tui.cursor < len(tui.filtered)-1 {
			tui.cursor++
		}

	case " ":
		tui.toggleCursor()

	case "a":
		for _, i := range tui.filtered {
			tui.selected[tui.procs[i].pid] = true
		}
		tui.message = fmt.Sprintf("Selected all %d visible processes", len(tui.filtered))

	case "A", "esc":
		tui.selected = make(map[int]bool)
		tui.search = ""
		tui.refilter()
		tui.message = "Selection and filter cleared"

	case "k":
		if len(tui.selected) == 0 {
			tui.message = "Nothing selected — use Space to select processes"
		} else {
			tui.mode = modeConfirm
		}

	case "/":
		tui.mode = modeSearch

	case "n":
		if tui.sortBy == sortByName {
			tui.sortDesc = !tui.sortDesc
		} else {
			tui.sortBy = sortByName
			tui.sortDesc = false
		}
		tui.sortProcs()
		tui.refilter()
		if tui.sortDesc {
			tui.message = "Sorted by name Z→A"
		} else {
			tui.message = "Sorted by name A→Z"
		}

	case "m":
		if tui.sortBy == sortByMem {
			tui.sortDesc = !tui.sortDesc
		} else {
			tui.sortBy = sortByMem
			tui.sortDesc = true
		}
		tui.sortProcs()
		tui.refilter()
		if tui.sortDesc {
			tui.message = "Sorted by memory high→low"
		} else {
			tui.message = "Sorted by memory low→high"
		}

	case "r":
		if err := tui.reload(); err != nil {
			tui.message = "Reload error: " + err.Error()
		}

	case "u":
		tui.showAll = !tui.showAll
		if err := tui.reload(); err != nil {
			tui.message = "Reload error: " + err.Error()
		} else if tui.showAll {
			tui.message = "Showing all processes"
		} else {
			tui.message = "Showing user processes"
		}
	}
	return false
}

func (tui *tui) handleSearchKey(key string) bool {
	switch key {
	case "ctrl+c", "ctrl+d":
		return true
	case "esc", "enter":
		tui.mode = modeNormal
	case "backspace":
		if len(tui.search) > 0 {
			r := []rune(tui.search)
			tui.search = string(r[:len(r)-1])
			tui.refilter()
		}
	default:
		if len(key) == 1 && key[0] >= 32 {
			tui.search += key
			tui.refilter()
		}
	}
	return false
}

func (tui *tui) handleConfirmKey(key string) bool {
	switch key {
	case "y", "Y":
		tui.mode = modeNormal
		killed, failed := 0, 0
		for pid := range tui.selected {
			if killPID(pid) == nil {
				killed++
			} else {
				failed++
			}
		}
		tui.selected = make(map[int]bool)
		if err := tui.reload(); err != nil {
			tui.message = "Reload error: " + err.Error()
			return false
		}
		tui.message = fmt.Sprintf("Killed %d process(es)", killed)
		if failed > 0 {
			tui.message += fmt.Sprintf(", %d failed (permission denied?)", failed)
		}
	case "n", "N", "esc", "enter", "q":
		tui.mode = modeNormal
		tui.message = "Kill cancelled"
	case "ctrl+c", "ctrl+d":
		return true
	}
	return false
}

func (tui *tui) toggleCursor() {
	if len(tui.filtered) == 0 {
		return
	}
	pid := tui.procs[tui.filtered[tui.cursor]].pid
	if tui.selected[pid] {
		delete(tui.selected, pid)
	} else {
		tui.selected[pid] = true
	}
}

func killPID(pid int) error {
	return unix.Kill(pid, unix.SIGKILL)
}

// ── Key reading ───────────────────────────────────────────────────────────────

func (tui *tui) readKey() string {
	b, err := tui.in.ReadByte()
	if err != nil {
		return "ctrl+d"
	}
	switch b {
	case 3:
		return "ctrl+c"
	case 4:
		return "ctrl+d"
	case 27: // ESC or ANSI escape sequence
		seq := tui.readEscSeq()
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
func (tui *tui) readEscSeq() string {
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
		b, err := tui.in.ReadByte()
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
	runes := []rune(s)
	if len(runes) >= n {
		return string(runes[:n])
	}
	return s + strings.Repeat(" ", n-len(runes))
}

// lpad left-pads s to at least n runes wide.
func lpad(s string, n int) string {
	w := utf8.RuneCountInString(s)
	if w >= n {
		return s
	}
	return strings.Repeat(" ", n-w) + s
}

// trunc truncates s to n bytes, adding "..." if it was longer.
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
