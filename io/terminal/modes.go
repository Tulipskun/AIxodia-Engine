package terminal

import (
	"fmt"
	"strconv"
	"strings"
)

// modeTracker remembers the DEC private modes a program leaves on.
//
// A session's ring buffer only holds the tail of its output. A full-screen
// program — opencode, vim, htop — set its mouse mode and alternate screen when
// it started, long before that tail. A client that attaches later would see the
// pixels but believe it is at a plain prompt: a swipe would scroll the client's
// own history instead of the program's. Replaying the modes it would have seen
// puts the client back in the state the program expects.
//
// Only the modes that change how input is encoded or which screen is drawn are
// kept. Synchronised output (2026) and other renderer hints are deliberately
// not replayed: restoring them mid-frame is worse than forgetting them.
type modeTracker struct {
	on map[int]bool

	// Parser state, carried across reads because a sequence can be split
	// between two writes from the pty.
	state     int
	params    []byte
	bad       bool
	stringOSC bool
	sawEsc    bool
}

const (
	modeGround = iota
	modeEscape
	modeCSI
	modeString
)

// trackedModes are the private modes that are remembered and replayed.
var trackedModes = map[int]bool{
	1: true, 7: true, 25: true, 47: true,
	1000: true, 1002: true, 1003: true, 1004: true,
	1005: true, 1006: true, 1015: true, 1016: true,
	1047: true, 1049: true, 2004: true,
}

// replayOrder matters: the alternate screen first so the ring buffer is drawn
// into it, then the modes that decide how input is encoded.
var replayOrder = []int{1049, 1047, 47, 1, 7, 25, 1000, 1002, 1003, 1004, 1005, 1006, 1015, 1016, 2004}

func newModeTracker() *modeTracker {
	return &modeTracker{on: map[int]bool{}}
}

// scan feeds pty output through the parser.
func (t *modeTracker) scan(b []byte) {
	for _, c := range b {
		switch t.state {
		case modeGround:
			if c == 0x1b {
				t.state = modeEscape
			}
		case modeEscape:
			switch c {
			case '[':
				t.state = modeCSI
				t.params = t.params[:0]
				t.bad = false
			case ']', 'P', '_', '^', 'X':
				t.state = modeString
				t.stringOSC = c == ']'
				t.sawEsc = false
			case 0x1b:
				// stay in escape: a second ESC starts a new sequence
			default:
				t.state = modeGround
			}
		case modeCSI:
			switch {
			case c >= 0x30 && c <= 0x3f:
				if len(t.params) < 64 {
					t.params = append(t.params, c)
				}
			case c >= 0x20 && c <= 0x2f:
				// An intermediate byte: sequences such as `ESC[?1016$p`
				// are queries, not mode changes.
				t.bad = true
			case c == 0x18 || c == 0x1a:
				t.state = modeGround
			case c >= 0x40 && c <= 0x7e:
				t.finishCSI(c)
				t.state = modeGround
			default:
				t.state = modeGround
			}
		case modeString:
			if t.sawEsc {
				t.sawEsc = false
				if c == '\\' {
					t.state = modeGround
				} else if c == 0x1b {
					t.sawEsc = true
				}
				// Any other byte continues the string.
			} else if c == 0x1b {
				t.sawEsc = true
			} else if c == 0x07 && t.stringOSC {
				t.state = modeGround
			}
		}
	}
}

func (t *modeTracker) finishCSI(final byte) {
	if t.bad || (final != 'h' && final != 'l') {
		return
	}
	p := t.params
	if len(p) < 2 || p[0] != '?' {
		return
	}
	on := final == 'h'
	for _, part := range strings.Split(string(p[1:]), ";") {
		n, err := strconv.Atoi(part)
		if err != nil || !trackedModes[n] {
			continue
		}
		t.on[n] = on
	}
}

// replay returns the escape sequences that put a fresh emulator into the modes
// the program has left on. Only non-default states need saying: a fresh
// terminal is already visible, wrapping and on the normal screen.
func (t *modeTracker) replay() []byte {
	var out []byte
	for _, m := range replayOrder {
		on, seen := t.on[m]
		if !seen {
			continue
		}
		switch m {
		case 25:
			if !on {
				out = append(out, "\x1b[?25l"...)
			}
		case 7:
			if !on {
				out = append(out, "\x1b[?7l"...)
			}
		default:
			if on {
				out = append(out, fmt.Sprintf("\x1b[?%dh", m)...)
			}
		}
	}
	return out
}
