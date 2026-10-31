package idle

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// fixed is a Detector that says what it is told.
type fixed struct {
	since time.Duration
	known bool
}

func (f fixed) Idle(context.Context) (time.Duration, bool) { return f.since, f.known }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	said, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return said
}

func TestModesAreGivenByName(t *testing.T) {
	for name, want := range map[string]Mode{"always": Always, "idle": WhenIdle, "paused": Paused} {
		got, err := ParseMode(name)
		if err != nil || got != want || got.String() != name {
			t.Errorf("%s: %v (%s), %v", name, got, got, err)
		}
	}
	for _, name := range []string{"", "Idle", "sometimes"} {
		if _, err := ParseMode(name); err == nil || !strings.Contains(err.Error(), "give always, idle or paused") {
			t.Errorf("%q: %v", name, err)
		}
	}
	if got := Mode(7).String(); got != "Mode(7)" {
		t.Errorf("an unknown mode is %q", got)
	}
}

// Each reason says why in words, for the worker's log.
func TestReasonsSayWhy(t *testing.T) {
	for reason, want := range map[Reason]string{
		Free: "lets it take", PausedByOwner: "paused", InUse: "is using it",
		NotKnown: "cannot tell", OnBattery: "battery", Reason(9): "Reason(9)",
	} {
		if !strings.Contains(reason.String(), want) {
			t.Errorf("%d says %q, want %q in it", int(reason), reason, want)
		}
	}
}

// Whether the machine takes new tasks, for each mode and what the owner is
// doing. Where it cannot tell, it takes the owner to be there.
func TestAPolicySaysWhenTheMachineTakesTasks(t *testing.T) {
	away, back := fixed{since: 10 * time.Minute, known: true}, fixed{since: 30 * time.Second, known: true}
	for _, tc := range []struct {
		name    string
		policy  Policy
		battery bool
		want    Reason
	}{
		{name: "always, owner there", policy: Policy{Mode: Always, Detector: back}, want: Free},
		{name: "always, not known", policy: Policy{Mode: Always, Detector: Unknown{}}, want: Free},
		{name: "always, on battery", policy: Policy{Mode: Always}, battery: true, want: Free},
		{name: "paused, owner away", policy: Policy{Mode: Paused, Detector: away}, want: PausedByOwner},
		{name: "idle, owner away long enough", policy: Policy{Mode: WhenIdle, After: 5 * time.Minute, Detector: away}, want: Free},
		{name: "idle, away exactly long enough", policy: Policy{Mode: WhenIdle, After: 10 * time.Minute, Detector: away}, want: Free},
		{name: "idle, owner back", policy: Policy{Mode: WhenIdle, After: 5 * time.Minute, Detector: back}, want: InUse},
		{name: "idle, away not long enough", policy: Policy{Mode: WhenIdle, After: 15 * time.Minute, Detector: away}, want: InUse},
		{name: "idle, nobody logged in", policy: Policy{Mode: WhenIdle, After: time.Hour, Detector: fixed{since: Forever, known: true}}, want: Free},
		{name: "idle, not known", policy: Policy{Mode: WhenIdle, After: 5 * time.Minute, Detector: Unknown{}}, want: NotKnown},
		{name: "idle, no detector", policy: Policy{Mode: WhenIdle, After: 5 * time.Minute}, want: NotKnown},
		{name: "idle, owner away but on battery", policy: Policy{Mode: WhenIdle, After: 5 * time.Minute, Detector: away}, battery: true, want: OnBattery},
		{name: "idle, owner away on mains", policy: Policy{Mode: WhenIdle, After: 5 * time.Minute, Detector: away}, want: Free},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.policy
			if tc.battery || strings.HasSuffix(tc.name, "on mains") {
				p.OnBattery = func() bool { return tc.battery }
			}
			if got := p.Check(context.Background()); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// programs answers as loginctl or ioreg would, from what it is given.
type programs struct {
	answers map[string][]byte
	fail    map[string]bool
	asked   [][]string
}

func (p *programs) run(_ context.Context, name string, args ...string) ([]byte, error) {
	p.asked = append(p.asked, append([]string{name}, args...))
	key := name + " " + args[0]
	if p.fail[key] {
		return nil, errors.New("exit status 1")
	}
	return p.answers[key], nil
}

func TestLogindSaysHowLongTheLeastIdleSessionHasBeenIdle(t *testing.T) {
	now := time.UnixMicro(1793430300000000)
	p := &programs{answers: map[string][]byte{
		"loginctl list-sessions": fixture(t, "list-sessions.txt"),
		"loginctl show-session":  fixture(t, "show-session.txt"),
	}}
	since, known := logind{run: p.run, now: func() time.Time { return now }}.Idle(context.Background())
	// The desktop went idle five minutes ago and the terminal twelve; the
	// login screen and the command with no terminal do not count.
	if !known || since != 5*time.Minute {
		t.Errorf("idle for %v (known %v), want 5m", since, known)
	}
	if got := p.asked[1]; !slices.Equal(got[len(got)-4:], []string{"2", "14", "c1", "187"}) || !slices.Contains(got, "--property=IdleSinceHint") {
		t.Errorf("asked %v", got)
	}
}

func TestLogindWithNobodyLoggedInIsIdleForever(t *testing.T) {
	p := &programs{answers: map[string][]byte{"loginctl list-sessions": []byte("\n")}}
	if since, known := (logind{run: p.run, now: time.Now}).Idle(context.Background()); !known || since != Forever {
		t.Errorf("idle for %v (known %v)", since, known)
	}
	if len(p.asked) != 1 {
		t.Errorf("asked %v", p.asked)
	}
}

func TestLogindThatCannotBeAskedDoesNotKnow(t *testing.T) {
	for _, failing := range []string{"loginctl list-sessions", "loginctl show-session"} {
		p := &programs{answers: map[string][]byte{"loginctl list-sessions": []byte("2 1000 ada seat0 tty2 active no -\n")}, fail: map[string]bool{failing: true}}
		if _, known := (logind{run: p.run, now: time.Now}).Idle(context.Background()); known {
			t.Errorf("%s failing, it is known", failing)
		}
	}
}

func TestSessionsIdle(t *testing.T) {
	now := time.UnixMicro(1793430300000000)
	session := func(lines ...string) string { return strings.Join(lines, "\n") + "\n" }
	desktop := func(hint, since string) string {
		return session("Class=user", "State=active", "Type=x11", "TTY=", "IdleHint="+hint, "IdleSinceHint="+since)
	}
	for _, tc := range []struct {
		name  string
		shown string
		since time.Duration
		known bool
	}{
		{name: "nothing said", shown: "", since: Forever, known: true},
		{name: "a desktop in use", shown: desktop("no", "0"), since: 0, known: true},
		{name: "a desktop idle a minute", shown: desktop("yes", "1793430240000000"), since: time.Minute, known: true},
		{name: "idle since later than now", shown: desktop("yes", "1793430360000000"), since: 0, known: true},
		{name: "idle with no time", shown: desktop("yes", "0"), known: false},
		{name: "idle with a time that is not one", shown: desktop("yes", "soon"), known: false},
		{name: "no idle hint", shown: session("Class=user", "Type=wayland"), known: false},
		{name: "one in use and one not known", shown: desktop("yes", "0") + "\n" + desktop("no", "0"), since: 0, known: true},
		{name: "a user-early desktop", shown: session("Class=user-early", "Type=wayland", "IdleHint=no"), since: 0, known: true},
		{name: "a closing session", shown: session("Class=user", "State=closing", "Type=x11", "IdleHint=no"), since: Forever, known: true},
		{name: "a terminal in use", shown: session("Class=user", "Type=tty", "TTY=pts/0", "IdleHint=no"), since: 0, known: true},
		{name: "a background command", shown: session("Class=user", "Type=unspecified", "TTY=", "IdleHint=no"), since: Forever, known: true},
		{name: "a login screen", shown: session("Class=greeter", "Type=wayland", "IdleHint=no"), since: Forever, known: true},
		{name: "blank lines either side", shown: "\n\n" + desktop("no", "0") + "\n\n", since: 0, known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			since, known := sessionsIdle([]byte(tc.shown), now)
			if known != tc.known || known && since != tc.since {
				t.Errorf("idle for %v (known %v), want %v (known %v)", since, known, tc.since, tc.known)
			}
		})
	}
}

func TestIoregSaysHowLongSinceTheLastInput(t *testing.T) {
	p := &programs{answers: map[string][]byte{"ioreg -c": fixture(t, "ioreg.txt")}}
	if since, known := (ioreg{run: p.run}).Idle(context.Background()); !known || since != 421337*time.Millisecond {
		t.Errorf("idle for %v (known %v)", since, known)
	}
	p.fail = map[string]bool{"ioreg -c": true}
	if _, known := (ioreg{run: p.run}).Idle(context.Background()); known {
		t.Error("ioreg failing, it is known")
	}
	for _, listed := range []string{"", `"HIDIdleTime" = soon`, `"HIDIdleTime" = -5`, `"HIDScrollAcceleration" = 20480`} {
		if _, known := hidIdle([]byte(listed)); known {
			t.Errorf("%q is known", listed)
		}
	}
}

func TestAMachineIsOnBatteryWhileItsBatteryDischarges(t *testing.T) {
	supply := func(kind, scope, status string) map[string]string {
		return map[string]string{"type": kind + "\n", "scope": scope + "\n", "status": status + "\n"}
	}
	for _, tc := range []struct {
		name     string
		supplies map[string]map[string]string
		want     bool
	}{
		{name: "no supplies"},
		{name: "a desktop on mains", supplies: map[string]map[string]string{"AC": supply("Mains", "", "")}},
		{name: "a laptop charging", supplies: map[string]map[string]string{"AC": supply("Mains", "", ""), "BAT0": supply("Battery", "", "Charging")}},
		{name: "a laptop full", supplies: map[string]map[string]string{"BAT0": supply("Battery", "", "Full")}},
		{name: "a laptop on battery", supplies: map[string]map[string]string{"AC": supply("Mains", "", ""), "BAT0": supply("Battery", "System", "Discharging")}, want: true},
		{name: "a second battery discharging", supplies: map[string]map[string]string{"BAT0": supply("Battery", "", "Not charging"), "BAT1": supply("Battery", "", "Discharging")}, want: true},
		{name: "a wireless mouse discharging", supplies: map[string]map[string]string{"AC": supply("Mains", "", ""), "hidpp_battery_0": supply("Battery", "Device", "Discharging")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := fstest.MapFS{}
			for name, files := range tc.supplies {
				for file, said := range files {
					tree[name+"/"+file] = &fstest.MapFile{Data: []byte(said)}
				}
			}
			if got := dischargingIn(tree); got != tc.want {
				t.Errorf("on battery %v, want %v", got, tc.want)
			}
		})
	}
}

// The system's own detector and battery answer, whatever they say: in a
// container, or with nobody logged in, they may not know.
func TestTheSystemsOwnAnswer(t *testing.T) {
	since, known := System().Idle(context.Background())
	if known && since < 0 {
		t.Errorf("idle for %v", since)
	}
	RunsOnBattery()
	if _, known := (Unknown{}).Idle(context.Background()); known {
		t.Error("Unknown knows")
	}
	if said, err := output(context.Background(), "go", "env", "GOOS"); err != nil || len(said) == 0 {
		t.Errorf("running a program: %q, %v", said, err)
	}
}
