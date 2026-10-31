package runtime

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Mining lends the worker's processor to someone who mines a coin to their
// own wallet: a miner connects to the mining pool the job names and
// submits shares under the payout address the job gives, for as long as
// the job's time box, and the pool pays that address. Sisyphus holds no
// keys and no coins and is never in the money's path: a payout address is
// public, and nothing secret is taken.
//
// The miner itself is an engine: an image of Sisyphus's own miner, from
// sisyphus-network/sisyphus-miner, built on the worker as the other fixed
// images are, which keeps to the contract MinerEngine sets out. No engine
// is built in yet, so for now no worker offers mining and a job of it is
// refused: everything around the miner is here, ready for it, and nothing
// mines.
//
// It is opt-in: a worker takes it only if its owner names it, or a group
// of its own, among the workloads to allow (see OptIn), since mining costs
// the owner power, heat and noise. Being in the containers group does not
// let it in.
//
// Each task's container has no network. The worker resolves the pool's
// name once, refuses any address that is not on the public Internet, and
// gives the container a Unix socket through which the worker connects to
// that one address and port, and to nothing else (see poolForwarder).
type Mining struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does. Its Network
	// is not used: a mining task reaches its pool and nothing else,
	// whether or not the worker's containers may reach the network.
	Confined Confinement

	// resolver, dial and refused stand in, in tests, for Go's resolver,
	// its dialer and poolAddressRefused. They are not a job's to set,
	// nor an owner's.
	resolver poolResolver
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
	refused  func(netip.Addr) error
}

// The groups mining is in. A worker's owner allows it by naming any of
// them, or mining itself: GroupMining for all mining, GroupMiningCPU for
// mining on the processor, and a group for each family of algorithm. The
// only algorithm yet is RandomX on the processor, so for now the three
// are the same; mining on graphics cards will be in groups of its own.
const (
	GroupMining        = "mining"
	GroupMiningCPU     = "mining-cpu"
	GroupMiningRandomX = "mining-randomx"
)

// MiningEgressLabel marks a worker that can run a miner held to its pool's
// one address: one on Linux, whose containers run with Docker's own
// runtime, with a miner engine for the job's algorithm. A Unix socket is
// how the worker reaches into a container that has no network; gVisor's
// and Kata's runtimes do not let a container connect to one on the
// machine, and nor does Docker Desktop, whose containers run in a virtual
// machine.
const MiningEgressLabel = "mining-egress"

// miningAlgorithms are the algorithms a job may name: RandomX for Monero.
// A job is taken only for one a miner engine mines.
var miningAlgorithms = []string{"rx/0"}

// MinerEngine is an image of a miner that a worker can run for a mining
// task, and the algorithms it mines. The worker builds it from Dockerfile
// the first time it is needed and tags it Image, which carries a hash of
// what it is made from, as the other fixed images' names do.
//
// The contract an engine keeps. Its image's own command is run, with no
// network, as the node's user with every capability dropped, and its root
// read-only where the owner asks. It is told, in its environment:
//
//   - SISYPHUS_POOL_SOCKET, the Unix socket (in /run/sisyphus-pool) its one
//     connection to the pool goes through: each connection made to it is
//     joined to the pool's address and port, and nothing else is reachable;
//   - SISYPHUS_POOL_HOST and SISYPHUS_POOL_PORT, the pool's name or address
//     and port as the job gave them, and SISYPHUS_POOL_TLS, true for
//     stratum+ssl, in which case the engine speaks TLS over the socket and
//     names SISYPHUS_POOL_HOST to the pool;
//   - SISYPHUS_ALGORITHM, SISYPHUS_ADDRESS (the payout address),
//     SISYPHUS_PASS (the pool's password field, never a secret) and
//     SISYPHUS_WORKER (the name to mine under);
//   - SISYPHUS_THREADS, how many threads to mine with, 0 for its own
//     choice; SISYPHUS_MEMORY_MB, the memory it is held to, 0 for no limit;
//     and SISYPHUS_DURATION, the seconds to mine for, after which it stops.
//
// It mines for the time box with no fee to anyone, and then prints, as
// the last line of its standard output, one JSON object (MinerSummary)
// and exits 0. What else it prints, on either stream, is the task's log.
type MinerEngine struct {
	Image      string
	Dockerfile string
	Algorithms []string
}

// MinerSummary is what an engine says of its time box, on its last line.
type MinerSummary struct {
	Algorithm string `json:"algorithm"`
	// Hashrate is hashes a second over the last HashrateWindow seconds,
	// and Highest the most it reached; nil if not measured.
	Hashrate       *float64 `json:"hashrate"`
	HashrateWindow int      `json:"hashrate_window"`
	Highest        *float64 `json:"hashrate_highest"`
	// Accepted and Rejected count the shares the pool took and refused,
	// and Connected is how many seconds it was connected to the pool.
	Accepted  uint64 `json:"accepted"`
	Rejected  uint64 `json:"rejected"`
	Connected int64  `json:"connected_seconds"`
	// FeePercent is the share of the time box mined for anyone but the
	// job's address, which must be said, and be 0.
	FeePercent *float64 `json:"fee_percent"`
}

// minerEngines are the miner engines this build of Sisyphus has. There
// are none yet: Sisyphus's own miner is to come from
// sisyphus-network/sisyphus-miner. Tests add a fake one.
var minerEngines []MinerEngine

// minerEngine returns the engine that mines algorithm, if there is one.
func minerEngine(algorithm string) (MinerEngine, bool) {
	for _, e := range minerEngines {
		if slices.Contains(e.Algorithms, algorithm) {
			return e, true
		}
	}
	return MinerEngine{}, false
}

// Bounds of a mining job: the shortest and longest a task may mine for,
// the longest a worker name and a pool password may be, and how long a
// task is given beyond its time box to start, say what it did and stop.
// Tests shorten the grace.
var miningGrace = 2 * time.Minute

const (
	miningShortest = time.Minute
	miningLongest  = 24 * time.Hour
	miningMostName = 32
	miningMostPass = 64
	// miningSocket is where the pool's socket is in the container.
	miningSocket = "/run/sisyphus-pool/pool.sock"
)

// MiningParams are a mining job's parameters.
type MiningParams struct {
	// Algorithm is what to mine with, one of miningAlgorithms. Empty
	// means rx/0.
	Algorithm string `json:"algorithm,omitempty"`
	// Pool is the mining pool's address, stratum+tcp://host:port, or
	// stratum+ssl://host:port to speak to it over TLS.
	Pool string `json:"pool"`
	// Address is the public address the pool pays: for rx/0, a Monero
	// address. No key, and nothing else secret, is ever asked for.
	Address string `json:"address"`
	// Worker is what each task calls itself to the pool, followed by a
	// dash, the job's and the task's number. Empty means sisyphus.
	Worker string `json:"worker,omitempty"`
	// Pass is what the pool is given as a password, which pools use for a
	// worker's name or an e-mail address, not as a secret. Empty means x.
	Pass string `json:"pass,omitempty"`
	// Duration is how long each task mines for, from 1m to 24h, as Go
	// writes a duration: "90m", "2h".
	Duration string `json:"duration"`
	// Tasks is how many tasks to mine with, each on a processor of its
	// own for the whole time box. Zero leaves it to the pool.
	Tasks int `json:"tasks,omitempty"`
}

// miningSettings are what a job asks of each task, checked.
type miningSettings struct {
	Algorithm string `json:"algorithm"`
	// Scheme is stratum+tcp or stratum+ssl, Host the pool's name or
	// address, and Port its port.
	Scheme  string `json:"scheme"`
	Host    string `json:"host"`
	Port    uint16 `json:"port"`
	Address string `json:"address"`
	Pass    string `json:"pass"`
	// Seconds is the time box.
	Seconds int `json:"seconds"`
}

// miningTask is one task's payload: the settings, and the name it mines
// under.
type miningTask struct {
	miningSettings
	Worker string `json:"worker"`
	Index  int    `json:"index"`
}

// MiningOutput is what one task says it did. Every number is its miner's
// own: self-reported.
type MiningOutput struct {
	Index     int    `json:"index"`
	Worker    string `json:"worker"`
	Algorithm string `json:"algorithm"`
	// Pool is the one address and port the task could reach.
	Pool string `json:"pool"`
	// Seconds is the time box it mined for, and Connected how long it was
	// connected to the pool.
	Seconds   int   `json:"seconds"`
	Connected int64 `json:"connected_seconds"`
	// Hashrate is hashes a second over the last HashrateWindow seconds,
	// and Highest the most it reached. Nil is not measured.
	Hashrate       *float64 `json:"hashrate"`
	HashrateWindow int      `json:"hashrate_window,omitempty"`
	Highest        *float64 `json:"hashrate_highest"`
	// Accepted and Rejected count the shares the pool took and refused.
	Accepted uint64 `json:"accepted"`
	Rejected uint64 `json:"rejected"`
}

// MiningResult is a mining job's result.
type MiningResult struct {
	// SelfReported is always true: what each task says it did is its own
	// miner's word. The pool's own statistics for the address are the
	// check of what was mined.
	SelfReported bool           `json:"self_reported"`
	Algorithm    string         `json:"algorithm"`
	Tasks        []MiningOutput `json:"tasks"`
	// Hashrate adds up the tasks' hashrates that were measured, and
	// Accepted and Rejected their shares.
	Hashrate float64 `json:"hashrate"`
	Accepted uint64  `json:"accepted"`
	Rejected uint64  `json:"rejected"`
}

func (Mining) Name() string { return "mining" }

func (Mining) Describe() string {
	return `Mines a coin to the requester's own wallet, on the processor, with Sisyphus's own miner: each task connects to the mining pool the job names and submits shares under the payout address it gives, for the time box, with no fee to anyone, and the pool pays that address. Nothing secret is taken and no coins pass through the pool of computers. Not available yet: no miner is built in, so no worker offers it and a job of it is refused. Only workers whose owners opted in to mining will take it, and each task can reach the mining pool's one address and nothing else; a pool on a private, loopback, link-local or CGNAT address is refused. Parameters: {"algorithm": "<rx/0 (RandomX, Monero), the default>", "pool": "<stratum+tcp://host:port, or stratum+ssl://host:port for TLS>", "address": "<the public Monero address the pool pays>", "worker": "<optional name prefix, letters, digits, dots, dashes and underscores, default sisyphus; each task adds -<job>-<n>>", "pass": "<optional pool password field, default x; never a secret>", "duration": "<how long each task mines, 1m to 24h, as 90m or 2h>", "tasks": <optional number of tasks>}. Result: {"self_reported": true, "algorithm": "rx/0", "tasks": [{"index": <n>, "worker": "<name>", "algorithm": "rx/0", "pool": "<address:port reached>", "seconds": <time box>, "connected_seconds": <n>, "hashrate": <hashes a second, or null>, "hashrate_window": <seconds it is over>, "hashrate_highest": <n or null>, "accepted": <shares>, "rejected": <shares>}], "hashrate": <sum>, "accepted": <n>, "rejected": <n>}. The numbers are each miner's own; the pool's statistics for the address are the real check.`
}

// OptIn says a worker takes mining only when its owner says so.
func (Mining) OptIn() bool { return true }

// Groups are mining's own groups, by which an owner may allow it.
func (Mining) Groups() []string { return []string{GroupMining, GroupMiningCPU, GroupMiningRandomX} }

// Varies says two tasks never mine the same: no result can be checked by
// running it again.
func (Mining) Varies() bool { return true }

// Needs says a mining task needs a worker that can run a miner held to its
// pool.
func (Mining) Needs([]byte) []string { return []string{MiningEgressLabel} }

// Offers is running a miner held to its pool, where this worker can and
// has a miner engine to run: nowhere yet.
func (m Mining) Offers(context.Context) []string {
	if m.canHold() && len(minerEngines) > 0 {
		return []string{MiningEgressLabel}
	}
	return nil
}

// miningOS is the operating system the worker runs on, as Go names it.
var miningOS = goruntime.GOOS

// canHold says whether this worker can hold a miner to its pool: whether
// a Unix socket of the worker's can be reached from inside a container.
func (m Mining) canHold() bool { return miningOS == "linux" && !m.Confined.sandboxed() }

func (Mining) Split(_ context.Context, _ Blobs, params []byte, parts int) ([][]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(params))
	// A parameter the workload does not know of is refused, not ignored:
	// a wallet's key, given by mistake, is not to be carried in a job.
	decoder.DisallowUnknownFields()
	var p MiningParams
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("mining parameters: %w", err)
	}
	settings, err := p.settings()
	if err != nil {
		return nil, fmt.Errorf("mining parameters: %w", err)
	}
	// Until there is a miner to run, a job of mining is refused here
	// rather than left waiting for a worker that offers it.
	if _, ok := minerEngine(settings.Algorithm); !ok {
		return nil, fmt.Errorf("mining parameters: mining %s is not available yet: Sisyphus's own miner, from sisyphus-network/sisyphus-miner, is not built in", settings.Algorithm)
	}
	worker := cmp.Or(p.Worker, "sisyphus")
	if err := checkMiningWorker(worker); err != nil {
		return nil, fmt.Errorf("mining parameters: %w", err)
	}
	if p.Tasks < 0 || p.Tasks > MaxTasks {
		return nil, fmt.Errorf("mining parameters: tasks is %d, and must be from 1 to %d, or 0 to leave it to the pool", p.Tasks, MaxTasks)
	}
	count := cmp.Or(p.Tasks, parts, 1)
	var job [4]byte
	rand.Read(job[:]) // never fails; see crypto/rand
	payloads := make([][]byte, count)
	for i := range payloads {
		payloads[i] = mustJSON(miningTask{miningSettings: settings, Worker: fmt.Sprintf("%s-%s-%d", worker, hex.EncodeToString(job[:]), i), Index: i})
	}
	return payloads, nil
}

// settings checks a job's parameters and makes its settings of them.
func (p MiningParams) settings() (miningSettings, error) {
	s := miningSettings{Algorithm: cmp.Or(p.Algorithm, miningAlgorithms[0]), Address: p.Address, Pass: cmp.Or(p.Pass, "x")}
	pool, err := url.Parse(p.Pool)
	if err != nil {
		return miningSettings{}, fmt.Errorf("pool %q is not a stratum+tcp:// or stratum+ssl:// address", p.Pool)
	}
	s.Scheme, s.Host = pool.Scheme, strings.ToLower(pool.Hostname())
	if port, err := strconv.ParseUint(pool.Port(), 10, 16); err == nil {
		s.Port = uint16(port)
	}
	duration, err := time.ParseDuration(p.Duration)
	if err != nil {
		return miningSettings{}, fmt.Errorf("duration %q is not a duration such as 90m or 2h", p.Duration)
	}
	if duration < miningShortest || duration > miningLongest || duration%time.Second != 0 {
		return miningSettings{}, fmt.Errorf("duration is %s, and must be a whole number of seconds from %s to %s", duration, miningShortest, miningLongest)
	}
	s.Seconds = int(duration / time.Second)
	if pool.User != nil || pool.Opaque != "" || strings.Trim(pool.Path, "/") != "" || pool.RawQuery != "" || pool.Fragment != "" {
		return miningSettings{}, fmt.Errorf("pool %q has more than a scheme, a host and a port", p.Pool)
	}
	return s, s.check()
}

// check refuses settings off the menu or out of bounds. A worker checks
// them again, since what it is handed came over the network and is put
// in its container's environment.
func (s miningSettings) check() error {
	switch {
	case !slices.Contains(miningAlgorithms, s.Algorithm):
		return fmt.Errorf("algorithm is %q, and must be one of %s", s.Algorithm, strings.Join(miningAlgorithms, ", "))
	case s.Scheme != "stratum+tcp" && s.Scheme != "stratum+ssl":
		return fmt.Errorf("pool's scheme is %q, and must be stratum+tcp or stratum+ssl", s.Scheme)
	case !poolHost(s.Host):
		return fmt.Errorf("pool's host %q is neither a name nor an address", s.Host)
	case s.Port == 0:
		return errors.New("pool has no port, or one that is not from 1 to 65535")
	case !moneroAddress(s.Address):
		return fmt.Errorf("address %q is not a Monero address: one of 95 characters starting with 4 or 8, or 106 starting with 4, in Monero's base58", s.Address)
	case len(s.Pass) > miningMostPass || strings.ContainsFunc(s.Pass, func(r rune) bool { return r <= ' ' || r > '~' }):
		return fmt.Errorf("pass may have at most %d letters, digits and punctuation, and no spaces", miningMostPass)
	case len(s.Pass) == 64 && strings.Trim(s.Pass, "0123456789abcdefABCDEF") == "":
		return errors.New("pass looks like a private key, 64 hexadecimal digits; a pool's password is never a secret, and no key is ever to be given to a job")
	case s.Seconds < int(miningShortest/time.Second) || s.Seconds > int(miningLongest/time.Second):
		return fmt.Errorf("the time box is %d seconds, and must be from %s to %s", s.Seconds, miningShortest, miningLongest)
	}
	return nil
}

// checkMiningWorker refuses a worker name a pool may not take, or that
// could be read as anything but a name.
func checkMiningWorker(name string) error {
	if name == "" || len(name) > miningMostName || strings.ContainsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_')
	}) {
		return fmt.Errorf("worker %q must be 1 to %d letters, digits, dots, dashes and underscores", name, miningMostName)
	}
	return nil
}

// poolHost reports whether host is an IP address or a name of the
// Internet's: labels of letters, digits and dashes, not beginning or
// ending with a dash, of at most 63 characters, at most 253 in all. A
// name is handed to the container engine, and nothing else may be.
func poolHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.ContainsFunc(label, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
		}) {
			return false
		}
	}
	return true
}

// moneroAlphabet is Monero's base58: digits and letters without 0, O, I
// and l.
const moneroAlphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// moneroAddress reports whether address has the shape of a Monero address
// on its main network: a standard address (95 characters, starting with
// 4), a subaddress (95, starting with 8) or an integrated address (106,
// starting with 4), in Monero's base58. Its checksum is not checked: a
// mistyped address is the pool's to refuse.
func moneroAddress(address string) bool {
	switch {
	case len(address) == 95 && (address[0] == '4' || address[0] == '8'):
	case len(address) == 106 && address[0] == '4':
	default:
		return false
	}
	return !strings.ContainsFunc(address, func(r rune) bool { return !strings.ContainsRune(moneroAlphabet, r) })
}

// env is the settings as a task's environment, as MinerEngine says.
func (t miningTask) env(confined Confinement) map[string]string {
	return map[string]string{
		"SISYPHUS_POOL_SOCKET": miningSocket, "SISYPHUS_POOL_HOST": t.Host, "SISYPHUS_POOL_PORT": strconv.Itoa(int(t.Port)),
		"SISYPHUS_POOL_TLS": strconv.FormatBool(t.Scheme == "stratum+ssl"), "SISYPHUS_ALGORITHM": t.Algorithm,
		"SISYPHUS_ADDRESS": t.Address, "SISYPHUS_PASS": t.Pass, "SISYPHUS_WORKER": t.Worker,
		"SISYPHUS_THREADS": strconv.Itoa(int(math.Ceil(confined.CPUs))), "SISYPHUS_MEMORY_MB": strconv.Itoa(confined.MemoryMB),
		"SISYPHUS_DURATION": strconv.Itoa(t.Seconds),
	}
}

func (m Mining) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task miningTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("mining payload: %w", err)
	}
	if err := task.check(); err != nil {
		return nil, fmt.Errorf("mining payload: %w", err)
	}
	if err := checkMiningWorker(task.Worker); err != nil {
		return nil, fmt.Errorf("mining payload: %w", err)
	}
	// A coordinator gives a mining task only to a worker that offers to
	// hold it to its pool, but this worker does not rely on that.
	if !m.canHold() {
		return nil, errors.New("this worker cannot hold a miner to its pool's address: that needs Docker's own runtime on Linux")
	}
	engine, ok := minerEngine(task.Algorithm)
	if !ok {
		return nil, fmt.Errorf("this worker has no miner for %s", task.Algorithm)
	}
	box := time.Duration(task.Seconds)*time.Second + miningGrace
	if m.Confined.Timeout > 0 && box > m.Confined.Timeout {
		return nil, fmt.Errorf("the task would mine for %s, and this worker's owner allows a task %s", time.Duration(task.Seconds)*time.Second, m.Confined.Timeout)
	}
	run := m.Engine
	if run == nil {
		run = docker
	}
	// The pool's address is found once, before anything is run or built,
	// and the task reaches that and nothing else.
	resolver, dial, refused := m.resolver, m.dial, m.refused
	if refused == nil {
		refused = poolAddressRefused
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	to, err := resolvePool(ctx, resolver, task.Host, task.Port, refused)
	if err != nil {
		return nil, err
	}
	if err := haveImage(ctx, run, engine.Image, engine.Dockerfile, "mining"); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(m.Confined.TaskDir, "sisyphus-pool-")
	if err != nil {
		return nil, fmt.Errorf("make the directory of the miner's socket: %w", err)
	}
	defer os.RemoveAll(dir)
	boxed, cancel := context.WithTimeout(ctx, box)
	defer cancel()
	forwarder, err := forwardPool(boxed, filepath.Join(dir, filepath.Base(miningSocket)), to, dial)
	if err != nil {
		return nil, err
	}
	report := Report(ctx)
	report.Log(fmt.Sprintf("mining %s as %s for %d seconds, reaching the pool %s at %s and nothing else", task.Algorithm, task.Worker, task.Seconds, task.Host, to))
	// What the miner prints is kept here too, for its last line.
	var printed capped
	keeping := func(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
		return run(ctx, args, io.MultiWriter(stdout, &printed), stderr)
	}
	_, err = Container{Engine: keeping, Confined: m.Confined, extra: []string{"--volume", dir + ":" + filepath.Dir(miningSocket)}}.Execute(boxed, blobs, mustJSON(containerTask{
		ContainerParams: ContainerParams{Image: engine.Image, Env: task.env(m.Confined)},
		Index:           task.Index, Count: 1,
	}))
	made, failed := forwarder.stop()
	report.Log(fmt.Sprintf("the miner made %d connections to its pool, and %d could not be made", made, failed))
	if err != nil {
		if boxed.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("the miner did not stop within %s of its time box, and was stopped", miningGrace)
		}
		return nil, err
	}
	summary, err := readMinerSummary(printed.Bytes())
	if err != nil {
		return nil, fmt.Errorf("the miner's summary: %w", err)
	}
	// A miner that never reached its pool mined for no one: the task
	// fails, rather than say it hashed for nothing.
	if made == 0 || summary.Connected <= 0 {
		return nil, fmt.Errorf("the miner never connected to its pool %s at %s (%d connections made, %d could not be made)", task.Host, to, made, failed)
	}
	output, err := summary.output(task)
	if err != nil {
		return nil, fmt.Errorf("the miner's summary: %w", err)
	}
	output.Pool = to.String()
	return mustJSON(output), nil
}

// readMinerSummary reads the summary on the last line a miner printed.
func readMinerSummary(printed []byte) (MinerSummary, error) {
	lines := strings.Split(strings.TrimSpace(string(printed)), "\n")
	var summary MinerSummary
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil {
		return MinerSummary{}, fmt.Errorf("its last line is not its summary: %w", err)
	}
	return summary, nil
}

// output makes a task's output of what its miner said, refusing what an
// engine that keeps to its contract would not say: a fee, another
// algorithm, or numbers that are no numbers of hashes or seconds.
func (s MinerSummary) output(task miningTask) (MiningOutput, error) {
	switch {
	case s.FeePercent == nil || *s.FeePercent != 0:
		return MiningOutput{}, errors.New("the miner does not say it mined with no fee")
	case s.Algorithm != task.Algorithm:
		return MiningOutput{}, fmt.Errorf("the miner mined %q, and the job asks for %s", s.Algorithm, task.Algorithm)
	case s.Hashrate != nil && (s.HashrateWindow <= 0 || s.HashrateWindow > task.Seconds):
		return MiningOutput{}, fmt.Errorf("the miner gives a hashrate over %d seconds of a time box of %d", s.HashrateWindow, task.Seconds)
	case s.Connected < 0 || s.Connected > int64(task.Seconds)+int64(miningGrace/time.Second):
		return MiningOutput{}, fmt.Errorf("the miner says it was connected for %d seconds of a time box of %d", s.Connected, task.Seconds)
	}
	for _, rate := range []*float64{s.Hashrate, s.Highest} {
		if rate != nil && !(*rate >= 0 && *rate < 1e15) {
			return MiningOutput{}, fmt.Errorf("the miner says it hashed %v times a second", *rate)
		}
	}
	out := MiningOutput{
		Index: task.Index, Worker: task.Worker, Algorithm: task.Algorithm, Seconds: task.Seconds, Connected: s.Connected,
		Hashrate: s.Hashrate, Highest: s.Highest, Accepted: s.Accepted, Rejected: s.Rejected,
	}
	if s.Hashrate != nil {
		out.HashrateWindow = s.HashrateWindow
	}
	return out, nil
}

// Aggregate puts the tasks' outputs together and adds them up. They are
// the workers' own word, and say so; each is checked only for being in its
// place and making sense.
func (Mining) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	result := MiningResult{SelfReported: true, Tasks: make([]MiningOutput, len(outputs))}
	for i, raw := range outputs {
		out := &result.Tasks[i]
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		if out.Index != i || (i > 0 && out.Algorithm != result.Tasks[0].Algorithm) {
			return nil, fmt.Errorf("task %d says it is task %d, mining %s", i, out.Index, out.Algorithm)
		}
		if out.Hashrate != nil {
			if !(*out.Hashrate >= 0 && *out.Hashrate < 1e15) {
				return nil, fmt.Errorf("task %d says it hashed %v times a second", i, *out.Hashrate)
			}
			result.Hashrate += *out.Hashrate
		}
		result.Accepted += out.Accepted
		result.Rejected += out.Rejected
	}
	if len(result.Tasks) > 0 {
		result.Algorithm = result.Tasks[0].Algorithm
	}
	return mustJSON(result), nil
}
