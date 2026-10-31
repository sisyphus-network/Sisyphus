package runtime

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// asIfOnLinux has the tests that run a miner run as on Linux, where a
// worker can hold one to its pool, wherever they run.
func asIfOnLinux(t *testing.T) {
	t.Helper()
	was := miningOS
	miningOS = "linux"
	t.Cleanup(func() { miningOS = was })
}

// testMoneroAddress has the shape of a Monero address and is nobody's: its
// checksum is wrong, so no wallet could have it.
var testMoneroAddress = "4" + strings.Repeat("A", 94)

// staticResolver resolves every name to the addresses it holds.
type staticResolver []netip.Addr

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	if len(r) == 0 {
		return nil, errors.New("no such host")
	}
	return r, nil
}

// onlyLoopback lets a test's miner reach a fake pool on this machine's
// loopback, which a job's miner never may; it is set only here, in a
// test, and is not a field a job or an owner can reach.
func onlyLoopback(addr netip.Addr) error {
	if addr.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%s is not this machine's loopback", addr)
}

// fakeEngine is the tests' miner engine. Its image is never built: the
// tests' fakeMiner stands in for Docker.
var fakeEngine = MinerEngine{Image: "sisyphus-test-miner:fake", Dockerfile: "FROM scratch\n", Algorithms: []string{"rx/0"}}

// withFakeEngine has this build of Sisyphus have a miner engine for the
// test, as none has yet.
func withFakeEngine(t *testing.T) {
	t.Helper()
	was := minerEngines
	minerEngines = []MinerEngine{fakeEngine}
	t.Cleanup(func() { minerEngines = was })
}

func miningParams(changes string) []byte {
	params := map[string]any{"pool": "stratum+tcp://pool.example.com:3333", "address": testMoneroAddress, "duration": "1h"}
	json.Unmarshal([]byte(changes), &params)
	for key, value := range params {
		if value == nil {
			delete(params, key)
		}
	}
	return mustJSON(params)
}

func TestMiningIsSplitIntoTimeBoxedTasks(t *testing.T) {
	withFakeEngine(t)
	ctx := context.Background()
	payloads, err := Mining{}.Split(ctx, nil, miningParams(`{"worker":"lab.rig_1","tasks":3,"pass":"me:x@example.com","duration":"90m"}`), 8)
	if err != nil || len(payloads) != 3 {
		t.Fatalf("%d payloads, %v", len(payloads), err)
	}
	var job string
	for i, payload := range payloads {
		var task miningTask
		json.Unmarshal(payload, &task)
		prefix, n, _ := strings.Cut(strings.TrimPrefix(task.Worker, "lab.rig_1-"), "-")
		if i == 0 {
			job = prefix
		}
		if task.Index != i || n != fmt.Sprint(i) || prefix != job || len(job) != 8 || task.Seconds != 5400 || task.Algorithm != "rx/0" ||
			task.Scheme != "stratum+tcp" || task.Host != "pool.example.com" || task.Port != 3333 || task.Address != testMoneroAddress || task.Pass != "me:x@example.com" {
			t.Errorf("task %d: %+v", i, task)
		}
	}
	// The pool's count of tasks, the defaults, and a pool's name in any case.
	payloads, err = Mining{}.Split(ctx, nil, miningParams(`{"pool":"stratum+ssl://Pool.Example.COM:443/","duration":"1m"}`), 2)
	var task miningTask
	if err == nil {
		json.Unmarshal(payloads[1], &task)
	}
	if len(payloads) != 2 || !strings.HasPrefix(task.Worker, "sisyphus-") || task.Pass != "x" || task.Scheme != "stratum+ssl" || task.Host != "pool.example.com" || task.Port != 443 || task.Seconds != 60 {
		t.Errorf("%d payloads, %+v, %v", len(payloads), task, err)
	}
	if payloads, err := (Mining{}).Split(ctx, nil, miningParams(`{"pool":"stratum+tcp://[2a00:1450::1]:3333"}`), 0); err != nil || len(payloads) != 1 {
		t.Errorf("an IPv6 pool, split in one: %d, %v", len(payloads), err)
	}
}

func TestMiningRefusesWhatItCannotTake(t *testing.T) {
	withFakeEngine(t)
	for _, tc := range []struct{ changes, want string }{
		{`{"algorithm":"kawpow"}`, `algorithm is "kawpow"`},
		{`{"pool":"http://pool.example.com:3333"}`, `scheme is "http"`},
		{`{"pool":"pool.example.com:3333"}`, "more than a scheme"},
		{`{"pool":"stratum+tcp://pool.example.com"}`, "no port"},
		{`{"pool":"stratum+tcp://pool.example.com:0"}`, "no port"},
		{`{"pool":"stratum+tcp://pool.example.com:70000"}`, "no port"},
		{`{"pool":"stratum+tcp://user:pw@pool.example.com:3333"}`, "more than a scheme"},
		{`{"pool":"stratum+tcp://pool.example.com:3333/x"}`, "more than a scheme"},
		{`{"pool":"stratum+tcp://pool.example.com:3333?a=b"}`, "more than a scheme"},
		{`{"pool":"stratum+tcp://pool.example.com:3333#a"}`, "more than a scheme"},
		{`{"pool":"stratum+tcp:pool.example.com:3333"}`, "more than a scheme"},
		{`{"pool":"stratum+tcp://-x.example.com:3333"}`, "neither a name nor an address"},
		{`{"pool":"stratum+tcp://x-.example.com:3333"}`, "neither a name nor an address"},
		{`{"pool":"stratum+tcp://pool..example.com:3333"}`, "neither a name nor an address"},
		{`{"pool":"stratum+tcp://pool_1.example.com:3333"}`, "neither a name nor an address"},
		{`{"pool":"stratum+tcp://` + strings.Repeat("a", 64) + `.com:3333"}`, "neither a name nor an address"},
		{`{"pool":"stratum+tcp://` + strings.Repeat("a.", 127) + `com:3333"}`, "neither a name nor an address"},
		{`{"pool":"stratum+tcp://:3333"}`, "neither a name nor an address"},
		{`{"pool":"%zz"}`, "is not a stratum"},
		{`{"pool":null}`, `scheme is ""`},
		{`{"address":null}`, "is not a Monero address"},
		{`{"address":"` + "4" + strings.Repeat("0", 94) + `"}`, "is not a Monero address"},
		{`{"address":"` + "5" + strings.Repeat("A", 94) + `"}`, "is not a Monero address"},
		{`{"address":"` + "8" + strings.Repeat("A", 105) + `"}`, "is not a Monero address"},
		{`{"address":"` + strings.Repeat("ab", 32) + `"}`, "is not a Monero address"},
		{`{"pass":"two words"}`, "no spaces"},
		{`{"pass":"` + strings.Repeat("p", 65) + `"}`, "at most 64"},
		{`{"pass":"` + strings.Repeat("e3", 32) + `"}`, "looks like a private key"},
		{`{"worker":"a rig"}`, `worker "a rig"`},
		{`{"worker":"` + strings.Repeat("w", 33) + `"}`, "1 to 32"},
		{`{"duration":null}`, `duration "" is not a duration`},
		{`{"duration":"an hour"}`, "is not a duration"},
		{`{"duration":"59s"}`, "from 1m0s to 24h0m0s"},
		{`{"duration":"24h1s"}`, "from 1m0s to 24h0m0s"},
		{`{"duration":"90.5s"}`, "whole number of seconds"},
		{`{"tasks":-1}`, "tasks is -1"},
		{`{"tasks":10001}`, "tasks is 10001"},
		// Nothing the workload does not know of is carried, a key least
		// of all.
		{`{"spend_key":"secret"}`, `unknown field "spend_key"`},
		{`{"duration":60}`, "cannot unmarshal number"},
	} {
		_, err := Mining{}.Split(context.Background(), nil, miningParams(tc.changes), 1)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "mining parameters: ") {
			t.Errorf("%s: %v, want %q", tc.changes, err, tc.want)
		}
	}
	// Subaddresses and integrated addresses are taken, as are names and
	// addresses of every kind; whether an address may be reached is the
	// worker's to say.
	for _, changes := range []string{
		`{"address":"8` + strings.Repeat("z", 94) + `"}`,
		`{"address":"4` + strings.Repeat("1", 105) + `"}`,
		`{"pool":"stratum+tcp://10.0.0.1:3333"}`,
		`{"pass":"` + strings.Repeat("e3", 31) + `"}`,
		`{"duration":"24h"}`,
	} {
		if _, err := (Mining{}).Split(context.Background(), nil, miningParams(changes), 1); err != nil {
			t.Errorf("%s: %v", changes, err)
		}
	}
}

// Mining is opt-in and in groups of its own: a worker whose owner says
// nothing never takes it, nor one whose owner allows every container
// workload; one whose owner names mining or a group of its does, unless a
// group it is in is denied.
func TestOnlyAnOwnerWhoOptsInTakesMining(t *testing.T) {
	asIfOnLinux(t)
	withFakeEngine(t)
	node := ContainersConfined(Confinement{})
	mining, _ := node.Get("mining")
	if got := GroupsOf(mining); !slices.Equal(got, []string{GroupContainers, GroupMining, GroupMiningCPU, GroupMiningRandomX}) {
		t.Errorf("mining is in %v", got)
	}
	for _, tc := range []struct {
		allow, deny []string
		takes       bool
	}{
		{nil, nil, false},
		{[]string{GroupDefault}, nil, false},
		{[]string{GroupContainers}, nil, false},
		{[]string{GroupDefault, GroupContainers, GroupModels}, nil, false},
		{[]string{"mining"}, nil, true},
		{[]string{GroupDefault, GroupMining}, nil, true},
		{[]string{GroupMiningCPU}, nil, true},
		{[]string{GroupMiningRandomX}, nil, true},
		{[]string{GroupMining}, []string{GroupContainers}, false},
		{[]string{GroupMining}, []string{GroupMiningRandomX}, false},
		{[]string{GroupMiningCPU}, []string{"mining"}, false},
	} {
		a, err := Accept(WithContainers(), tc.allow, tc.deny)
		if err != nil {
			t.Fatal(err)
		}
		if a.Accepts(mining) != tc.takes || slices.Contains(node.Accepted(a).Names(), "mining") != tc.takes {
			t.Errorf("allow %v, deny %v: takes mining %v", tc.allow, tc.deny, !tc.takes)
		}
	}
	// What a worker tells its pool: the default worker neither runs it nor
	// offers its label.
	if offers := node.Accepted(Acceptance{}).Offers(context.Background()); slices.Contains(offers, MiningEgressLabel) {
		t.Errorf("a default worker offers %v", offers)
	}
	// Mining needs a worker that can hold it to its pool, and never the
	// network.
	if needs := node.Needs("mining", miningParams(`{}`)); !slices.Equal(needs, []string{MiningEgressLabel}) {
		t.Errorf("mining needs %v", needs)
	}
	if !node.InContainers("mining") || Checkable(mining) {
		t.Error("mining runs in containers, and no run of it can be checked by another")
	}
}

// Until Sisyphus has a miner of its own built in, no worker offers mining,
// wherever it runs, and a job of it is refused when it is submitted.
func TestNoOneMinesUntilThereIsAMiner(t *testing.T) {
	asIfOnLinux(t)
	ctx := context.Background()
	if len(minerEngines) != 0 {
		t.Fatalf("this build has miner engines %v", minerEngines)
	}
	if offers := (Mining{}).Offers(ctx); offers != nil {
		t.Errorf("a worker with no miner offers %v", offers)
	}
	if _, err := (Mining{}).Split(ctx, nil, miningParams(`{}`), 1); err == nil || !strings.Contains(err.Error(), "mining rx/0 is not available yet: Sisyphus's own miner, from sisyphus-network/sisyphus-miner, is not built in") {
		t.Errorf("a job with no miner to run it: %v", err)
	}
	// A worker built without one, handed a task by a coordinator built with
	// one, says so.
	withFakeEngine(t)
	payloads, _ := Mining{}.Split(ctx, nil, miningParams(`{}`), 1)
	minerEngines = nil
	if _, err := (Mining{Engine: (&fakeMiner{t: t}).engine}).Execute(ctx, nil, payloads[0]); err == nil || !strings.Contains(err.Error(), "this worker has no miner for rx/0") {
		t.Errorf("a task with no miner to run it: %v", err)
	}
}

func TestWhereAMinerCanBeHeldToItsPool(t *testing.T) {
	withFakeEngine(t)
	ctx := context.Background()
	defer func(os string) { miningOS = os }(miningOS)
	for _, tc := range []struct {
		os       string
		confined Confinement
		offers   bool
	}{
		{"linux", Confinement{}, true},
		{"linux", Confinement{Runtime: "runc", Network: true}, true},
		{"linux", Confinement{Runtime: "runsc"}, false},
		{"linux", Confinement{Runtime: "io.containerd.kata.v2"}, false},
		{"darwin", Confinement{}, false},
		{"windows", Confinement{}, false},
	} {
		miningOS = tc.os
		m := Mining{Confined: tc.confined, Engine: func(context.Context, []string, io.Writer, io.Writer) (int, error) {
			t.Fatal("a worker that cannot hold a miner ran one")
			return 0, nil
		}}
		if got := slices.Contains(m.Offers(ctx), MiningEgressLabel); got != tc.offers {
			t.Errorf("%s with %+v offers to hold a miner: %v", tc.os, tc.confined, got)
		}
		if tc.offers {
			continue
		}
		payloads, _ := m.Split(ctx, nil, miningParams(`{}`), 1)
		if _, err := m.Execute(ctx, nil, payloads[0]); err == nil || !strings.Contains(err.Error(), "cannot hold a miner") {
			t.Errorf("%s with %+v: %v", tc.os, tc.confined, err)
		}
	}
}

// fakeMiner stands in for Docker and a miner engine. It builds nothing;
// running a task, it reaches the pool through the socket it is given, as
// an engine does, logs in and submits a share, and prints its summary as
// its last line.
type fakeMiner struct {
	t *testing.T
	// summary is the last line it prints, and status what it exits with.
	// Block makes it wait for its task to be stopped, noImage have no
	// image and fail to build one, and offline not reach its pool.
	summary string
	status  int
	block   bool
	noImage bool
	offline bool

	mu    sync.Mutex
	runs  [][]string
	heard []string
}

// fakeSummary is what a fake engine says of 20 seconds at the fake pool.
const fakeSummary = `{"algorithm":"rx/0","hashrate":34.5,"hashrate_window":10,"hashrate_highest":37.35,"accepted":61,"rejected":0,"connected_seconds":20,"fee_percent":0}`

func (f *fakeMiner) engine(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	switch args[0] {
	case "image":
		if f.noImage {
			return 1, nil
		}
		return 0, nil
	case "build":
		fmt.Fprintln(stderr, "no network to build with")
		return 1, nil
	case "kill":
		return 0, nil
	}
	f.mu.Lock()
	f.runs = append(f.runs, args)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return 137, nil
	}
	mounts := map[string]string{}
	env := map[string]string{}
	for i, arg := range args {
		switch arg {
		case "--volume":
			from, to, _ := strings.Cut(args[i+1], ":")
			mounts[strings.TrimSuffix(to, ":ro")] = from
		case "--env":
			key, value, _ := strings.Cut(args[i+1], "=")
			env[key] = value
		}
	}
	if !f.offline {
		socket := filepath.Join(mounts[filepath.Dir(env["SISYPHUS_POOL_SOCKET"])], filepath.Base(env["SISYPHUS_POOL_SOCKET"]))
		conn, err := net.Dial("unix", socket)
		if err != nil {
			f.t.Errorf("the miner could not reach its socket: %v", err)
			return 1, nil
		}
		lines := bufio.NewScanner(conn)
		for _, call := range []string{
			`{"id":1,"jsonrpc":"2.0","method":"login","params":{"login":"` + env["SISYPHUS_ADDRESS"] + `","pass":"` + env["SISYPHUS_PASS"] + `","agent":"fake","rigid":"` + env["SISYPHUS_WORKER"] + `"}}`,
			`{"id":2,"jsonrpc":"2.0","method":"submit","params":{"id":"fake-session","job_id":"fake-1","nonce":"00000000","result":"00"}}`,
		} {
			fmt.Fprintln(conn, call)
			if lines.Scan() {
				f.mu.Lock()
				f.heard = append(f.heard, lines.Text())
				f.mu.Unlock()
			}
		}
		conn.Close()
	}
	fmt.Fprintln(stdout, "mining rx/0, no fee")
	fmt.Fprintln(stdout, f.summary)
	return f.status, nil
}

// A task's miner runs in a container with no network, held to the address
// its pool's name resolved to, reached through the socket it is given:
// what it says to the pool, and what the pool answers, pass through.
func TestAMinerReachesItsPoolAndNothingElse(t *testing.T) {
	asIfOnLinux(t)
	withFakeEngine(t)
	ctx := context.Background()
	pool := startFakePool(t)
	port := pool.listener.Addr().(*net.TCPAddr).Port
	fake := &fakeMiner{t: t, summary: fakeSummary}
	var dialed []string
	var mu sync.Mutex
	m := Mining{
		Engine:   fake.engine,
		Confined: Confinement{TaskDir: t.TempDir(), MemoryMB: 2048, CPUs: 1.5, Network: true, Timeout: time.Hour},
		resolver: staticResolver{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("127.0.0.1")},
		refused:  onlyLoopback,
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, address)
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	store := storage.NewMemory()
	payloads, err := m.Split(ctx, store, miningParams(fmt.Sprintf(`{"pool":"stratum+tcp://pool.fake.test:%d","worker":"rig","duration":"10m"}`, port)), 1)
	if err != nil {
		t.Fatal(err)
	}
	said := new(heard)
	raw, err := m.Execute(WithReporter(ctx, said), store, payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	var out MiningOutput
	json.Unmarshal(raw, &out)
	want := fmt.Sprintf("127.0.0.1:%d", port)
	if out.Pool != want || out.Index != 0 || !strings.HasPrefix(out.Worker, "rig-") || out.Seconds != 600 || out.Connected != 20 || out.Accepted != 61 || out.Rejected != 0 ||
		*out.Hashrate != 34.5 || out.HashrateWindow != 10 || *out.Highest != 37.35 || out.Algorithm != "rx/0" {
		t.Errorf("the output: %s", raw)
	}
	logins, shares := pool.seen()
	if len(logins) != 1 || logins[0]["login"] != testMoneroAddress || logins[0]["rigid"] != out.Worker || shares != 1 || len(fake.heard) != 2 || !strings.Contains(fake.heard[0], `"job_id":"fake-1"`) {
		t.Errorf("the pool saw %v and %d shares; the miner heard %v", logins, shares, fake.heard)
	}
	if !slices.Equal(dialed, []string{want}) {
		t.Errorf("the worker dialed %v", dialed)
	}
	args := strings.Join(fake.runs[0], " ")
	for _, want := range []string{"--network none", ":/run/sisyphus-pool " + fakeEngine.Image, "--memory 2048m", "--cpus 1.5",
		"--env SISYPHUS_ADDRESS=" + testMoneroAddress, "--env SISYPHUS_ALGORITHM=rx/0", "--env SISYPHUS_DURATION=600", "--env SISYPHUS_MEMORY_MB=2048",
		"--env SISYPHUS_PASS=x", "--env SISYPHUS_POOL_HOST=pool.fake.test", fmt.Sprintf("--env SISYPHUS_POOL_PORT=%d", port),
		"--env SISYPHUS_POOL_SOCKET=/run/sisyphus-pool/pool.sock", "--env SISYPHUS_POOL_TLS=false", "--env SISYPHUS_THREADS=2", "--env SISYPHUS_WORKER=" + out.Worker} {
		if !strings.Contains(args, want) {
			t.Errorf("the container is run without %q: %s", want, args)
		}
	}
	if strings.Count(args, "--network") != 1 || !strings.HasSuffix(args, " "+fakeEngine.Image) {
		t.Errorf("the container is given a network, or a command of the job's: %s", args)
	}
	log := strings.Join(said.lines, "\n")
	if !strings.Contains(log, "reaching the pool pool.fake.test at "+want+" and nothing else") || !strings.Contains(log, "made 1 connections to its pool, and 0 could not be made") || !strings.Contains(log, "mining rx/0, no fee") {
		t.Errorf("the log:\n%s", log)
	}
	if left, _ := os.ReadDir(m.Confined.TaskDir); len(left) != 0 {
		t.Errorf("the task left %v behind", left)
	}

	// A pool named by its address is reached at it; over TLS, with no
	// limits, the miner works out its own threads and memory.
	m.Confined = Confinement{}
	payloads, _ = m.Split(ctx, store, miningParams(fmt.Sprintf(`{"pool":"stratum+ssl://127.0.0.1:%d"}`, port)), 1)
	if _, err := m.Execute(ctx, store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	args = strings.Join(fake.runs[1], " ")
	if !strings.Contains(args, "--env SISYPHUS_POOL_HOST=127.0.0.1") || !strings.Contains(args, "--env SISYPHUS_POOL_TLS=true") ||
		!strings.Contains(args, "--env SISYPHUS_MEMORY_MB=0") || !strings.Contains(args, "--env SISYPHUS_THREADS=0") {
		t.Errorf("the container is run as %s", args)
	}
}

// A job cannot point a miner at the worker's own network: a pool whose
// name resolves only there is refused before anything is run or built.
func TestAMinerIsNeverPointedAtTheOwnersNetwork(t *testing.T) {
	asIfOnLinux(t)
	withFakeEngine(t)
	ctx := context.Background()
	// A pool named by its address is not resolved, and its worker's own
	// resolver and engine are left in place.
	for _, tc := range []struct {
		pool     string
		resolves poolResolver
		want     string
	}{
		{"stratum+tcp://pool.example.com:3333", staticResolver{netip.MustParseAddr("127.0.0.1")}, "127.0.0.1 is not an address on the public Internet"},
		{"stratum+tcp://router.example.com:80", staticResolver{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("fd00::1")}, "has no address a miner may reach"},
		{"stratum+tcp://nas.example.com:445", staticResolver{}, "find the pool nas.example.com: no such host"},
		{"stratum+tcp://100.100.100.100:3333", nil, "100.100.100.100 is not an address on the public Internet (it is in 100.64.0.0/10)"},
		{"stratum+tcp://[::ffff:10.0.0.1]:3333", nil, "10.0.0.1 is not an address on the public Internet"},
	} {
		fake := &fakeMiner{t: t, noImage: true}
		m := Mining{Engine: fake.engine, resolver: tc.resolves}
		if tc.resolves == nil {
			m.Engine = nil
		}
		payloads, err := m.Split(ctx, nil, miningParams(`{"pool":"`+tc.pool+`"}`), 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Execute(ctx, nil, payloads[0]); err == nil || !strings.Contains(err.Error(), tc.want) || len(fake.runs) != 0 {
			t.Errorf("%s: %v, and ran %d containers", tc.pool, err, len(fake.runs))
		}
	}
}

func TestAMiningTaskThatCannotRunSaysWhy(t *testing.T) {
	asIfOnLinux(t)
	withFakeEngine(t)
	ctx := context.Background()
	defer func(grace time.Duration) { miningGrace = grace }(miningGrace)
	pool := startFakePool(t)
	poolAt := fmt.Sprintf(`{"pool":"stratum+tcp://pool.fake.test:%d"}`, pool.listener.Addr().(*net.TCPAddr).Port)
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	os.Mkdir(long, 0o700)
	payload := func(changes string) []byte {
		payloads, err := Mining{}.Split(ctx, nil, miningParams(changes), 1)
		if err != nil {
			t.Fatal(err)
		}
		return payloads[0]
	}
	never := strings.Replace(fakeSummary, `"connected_seconds":20`, `"connected_seconds":0`, 1)
	for _, tc := range []struct {
		name     string
		fake     *fakeMiner
		confined Confinement
		payload  []byte
		want     string
	}{
		{name: "a payload that is not one", payload: []byte("{"), want: "mining payload: unexpected end"},
		{name: "settings off the menu", payload: []byte(`{"algorithm":"rx/0","scheme":"http"}`), want: `mining payload: pool's scheme is "http"`},
		{name: "no time box", payload: []byte(strings.Replace(string(payload(`{}`)), `"seconds":3600`, `"seconds":0`, 1)), want: "mining payload: the time box is 0 seconds"},
		{name: "a worker name that is not one", payload: []byte(strings.Replace(string(payload(`{}`)), `"worker":"sisyphus-`, `"worker":"--privileged sisyphus-`, 1)), want: `mining payload: worker "--privileged`},
		{name: "a time box longer than the owner allows", payload: payload(`{}`), confined: Confinement{Timeout: time.Hour}, want: "would mine for 1h0m0s, and this worker's owner allows a task 1h0m0s"},
		{name: "an image that cannot be built", fake: &fakeMiner{noImage: true}, payload: payload(poolAt), want: "build the image mining runs in: no network to build with"},
		{name: "nowhere for the socket", payload: payload(poolAt), confined: Confinement{TaskDir: filepath.Join(t.TempDir(), "gone")}, want: "make the directory of the miner's socket"},
		{name: "a socket's path too long", payload: payload(poolAt), confined: Confinement{TaskDir: long}, want: "make the socket the miner reaches its pool through"},
		{name: "a miner that fails", fake: &fakeMiner{status: 1, summary: fakeSummary}, payload: payload(poolAt), want: "the command exited with status 1"},
		{name: "no summary", fake: &fakeMiner{}, payload: payload(poolAt), want: "the miner's summary: its last line is not its summary"},
		{name: "a summary that is not one", fake: &fakeMiner{summary: "{"}, payload: payload(poolAt), want: "its last line is not its summary: unexpected end"},
		{name: "a fee", fake: &fakeMiner{summary: strings.Replace(fakeSummary, `"fee_percent":0`, `"fee_percent":1`, 1)}, payload: payload(poolAt), want: "does not say it mined with no fee"},
		{name: "a miner that never reached its pool", fake: &fakeMiner{offline: true, summary: fakeSummary}, payload: payload(poolAt), want: "the miner never connected to its pool pool.fake.test at 127.0.0.1:"},
		{name: "a miner that says it never connected", fake: &fakeMiner{summary: never}, payload: payload(poolAt), want: "never connected to its pool"},
		{name: "a miner that outstays its time box", fake: &fakeMiner{block: true}, payload: payload(poolAt), want: "the miner did not stop within"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			miningGrace = 2 * time.Minute
			if tc.fake != nil && tc.fake.block {
				miningGrace = -59*time.Minute - 59*time.Second - 900*time.Millisecond
			}
			fake := cmp.Or(tc.fake, &fakeMiner{summary: fakeSummary})
			fake.t = t
			m := Mining{Engine: fake.engine, Confined: tc.confined, resolver: staticResolver{netip.MustParseAddr("127.0.0.1")}, refused: onlyLoopback}
			if _, err := m.Execute(ctx, storage.NewMemory(), tc.payload); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v, want %q", err, tc.want)
			}
		})
	}
	// A task stopped by its coordinator is stopped, and says so.
	stopped, stop := context.WithCancel(ctx)
	fake := &fakeMiner{t: t, block: true}
	m := Mining{Engine: fake.engine, resolver: staticResolver{netip.MustParseAddr("127.0.0.1")}, refused: onlyLoopback}
	go func() {
		for {
			fake.mu.Lock()
			ran := len(fake.runs)
			fake.mu.Unlock()
			if ran > 0 {
				stop()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if _, err := m.Execute(stopped, storage.NewMemory(), payload(poolAt)); !errors.Is(err, context.Canceled) {
		t.Errorf("a stopped task: %v", err)
	}
}

// What a task makes of its miner's summary, and what it refuses: what an
// engine that keeps to its contract would not say.
func TestAMinersSummaryIsReadForWhatItSays(t *testing.T) {
	task := miningTask{miningSettings: miningSettings{Algorithm: "rx/0", Seconds: 60}, Worker: "rig-1", Index: 2}
	read := func(change string) (MiningOutput, error) {
		var s map[string]any
		json.Unmarshal([]byte(fakeSummary), &s)
		json.Unmarshal([]byte(change), &s)
		summary, err := readMinerSummary(append([]byte("starting\n"), mustJSON(s)...))
		if err != nil {
			t.Fatal(err)
		}
		return summary.output(task)
	}
	out, err := read(`{"hashrate":31.25,"hashrate_window":60,"hashrate_highest":40}`)
	if err != nil || *out.Hashrate != 31.25 || out.HashrateWindow != 60 || *out.Highest != 40 || out.Index != 2 || out.Worker != "rig-1" || out.Seconds != 60 || out.Connected != 20 || out.Accepted != 61 {
		t.Errorf("%+v, %v", out, err)
	}
	// One that never measured its hashrate.
	out, err = read(`{"hashrate":null,"hashrate_window":0,"hashrate_highest":null}`)
	if err != nil || out.Hashrate != nil || out.Highest != nil || out.HashrateWindow != 0 {
		t.Errorf("not measured: %+v, %v", out, err)
	}
	for _, tc := range []struct{ change, want string }{
		{`{"fee_percent":null}`, "no fee"},
		{`{"fee_percent":0.5}`, "no fee"},
		{`{"algorithm":"rx/wow"}`, `the miner mined "rx/wow", and the job asks for rx/0`},
		{`{"algorithm":null}`, `the miner mined ""`},
		{`{"hashrate_window":0}`, "a hashrate over 0 seconds"},
		{`{"hashrate_window":61}`, "a hashrate over 61 seconds of a time box of 60"},
		{`{"hashrate":-1}`, "hashed -1 times a second"},
		{`{"hashrate_highest":1e16}`, "hashed 1e+16 times"},
		{`{"connected_seconds":-1}`, "connected for -1 seconds"},
		{`{"connected_seconds":1000}`, "connected for 1000 seconds of a time box of 60"},
	} {
		if _, err := read(tc.change); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.change, err, tc.want)
		}
	}
}

func TestMiningResultsAreAddedUpAndSaySoAreSelfReported(t *testing.T) {
	ctx := context.Background()
	rate := func(r float64) *float64 { return &r }
	outputs := [][]byte{
		mustJSON(MiningOutput{Index: 0, Algorithm: "rx/0", Hashrate: rate(30.5), Accepted: 10, Rejected: 1}),
		mustJSON(MiningOutput{Index: 1, Algorithm: "rx/0", Accepted: 0}),
		mustJSON(MiningOutput{Index: 2, Algorithm: "rx/0", Hashrate: rate(12), Accepted: 4}),
	}
	raw, err := Mining{}.Aggregate(ctx, nil, outputs)
	var result MiningResult
	json.Unmarshal(raw, &result)
	if err != nil || !result.SelfReported || result.Algorithm != "rx/0" || result.Hashrate != 42.5 || result.Accepted != 14 || result.Rejected != 1 || len(result.Tasks) != 3 {
		t.Errorf("%s, %v", raw, err)
	}
	if raw, err := (Mining{}).Aggregate(ctx, nil, nil); err != nil || !strings.Contains(string(raw), `"self_reported":true`) {
		t.Errorf("no tasks: %s, %v", raw, err)
	}
	for _, bad := range [][][]byte{
		{[]byte("{")},
		{mustJSON(MiningOutput{Index: 1, Algorithm: "rx/0"})},
		{outputs[0], mustJSON(MiningOutput{Index: 1, Algorithm: "rx/wow"})},
		{mustJSON(MiningOutput{Index: 0, Algorithm: "rx/0", Hashrate: rate(-2)})},
	} {
		if _, err := (Mining{}).Aggregate(ctx, nil, bad); err == nil {
			t.Errorf("%s is taken", bad)
		}
	}
}

func TestMiningDescribesItself(t *testing.T) {
	described := WithContainers().Describe("mining")
	for _, want := range []string{"own wallet", "no fee", "Not available yet", "opted in", "Nothing secret", "self_reported", "private, loopback, link-local or CGNAT"} {
		if !strings.Contains(described, want) {
			t.Errorf("mining's description says nothing of %q", want)
		}
	}
}
