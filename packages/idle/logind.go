package idle

import (
	"bufio"
	"bytes"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// logind tells how long the machine's owner has been away from what
// systemd-logind says of their sessions, by asking loginctl. A desktop says
// its session is idle once its own screensaver's delay has passed with no
// input, so the shortest idleness logind can tell is that delay; a desktop
// that never says so is taken to be in use. A text session is idle by when
// its terminal was last used.
type logind struct {
	// run runs a program and returns what it printed; now is the time.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	now func() time.Time
	// users, if not empty, are the accounts whose sessions count: those
	// of any other, such as a server's automation account with a tmux
	// attached on a terminal, are passed over.
	users []string
}

// Idle is how long the least idle of the users' sessions has been idle:
// Forever if nobody is logged in, and not known if loginctl cannot be asked
// or says nothing of when an idle session went idle.
func (l logind) Idle(ctx context.Context) (time.Duration, bool) {
	listed, err := l.run(ctx, "loginctl", "list-sessions", "--no-legend")
	if err != nil {
		return 0, false
	}
	var ids []string
	for line := range strings.Lines(string(listed)) {
		if fields := strings.Fields(line); len(fields) > 0 {
			ids = append(ids, fields[0])
		}
	}
	if len(ids) == 0 {
		return Forever, true
	}
	shown, err := l.run(ctx, "loginctl", append([]string{"show-session",
		"--property=Name", "--property=Class", "--property=State", "--property=Type", "--property=TTY", "--property=IdleHint", "--property=IdleSinceHint"}, ids...)...)
	if err != nil {
		return 0, false
	}
	return sessionsIdle(shown, l.now(), l.users)
}

// sessionsIdle reads what `loginctl show-session` says of sessions: for
// each, its properties a line each as Name=value, with a blank line between
// sessions. Only those of someone at a screen or a terminal count: of class
// user, or a kind of it such as user-early, and not a login screen's; with a
// display or a terminal, and not a command run over SSH or by a timer,
// which logind always says is in use; not one left closing when its user
// logged out; and, if users are given, of one of them by account name.
func sessionsIdle(shown []byte, now time.Time, users []string) (time.Duration, bool) {
	least, known := Forever, true
	session := map[string]string{}
	end := func() {
		defer clear(session)
		if len(session) == 0 {
			return
		}
		if class := session["Class"]; class != "user" && !strings.HasPrefix(class, "user-") || session["State"] == "closing" {
			return
		}
		if len(users) > 0 && !slices.Contains(users, session["Name"]) {
			return
		}
		switch session["Type"] {
		case "x11", "wayland", "mir":
		default:
			if session["TTY"] == "" {
				return
			}
		}
		switch session["IdleHint"] {
		case "no":
			least = 0
			return
		case "yes":
			since, err := strconv.ParseInt(session["IdleSinceHint"], 10, 64)
			if err == nil && since > 0 {
				least = min(least, max(0, now.Sub(time.UnixMicro(since))))
				return
			}
		}
		known = false
	}
	lines := bufio.NewScanner(bytes.NewReader(shown))
	for lines.Scan() {
		name, value, ok := strings.Cut(strings.TrimSpace(lines.Text()), "=")
		if !ok {
			end()
			continue
		}
		session[name] = value
	}
	end()
	if least == 0 {
		// Someone is using the machine, whatever else is not known.
		return 0, true
	}
	if !known {
		return 0, false
	}
	return least, true
}
