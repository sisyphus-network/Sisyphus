package runtime

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	"github.com/ipfs/go-cid"
)

// Mining lends the worker's processor to someone who mines a coin to their
// own wallet: XMRig, built from source with no donation to its authors,
// connects to the mining pool the job names and submits shares under the
// payout address the job gives, for as long as the job's time box. The
// pool pays that address. Sisyphus holds no keys and no coins and is never
// in the money's path: a payout address is public, and nothing secret is
// taken.
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

// MiningEgressLabel marks a worker that can hold a miner to its pool's one
// address: one on Linux, whose containers run with Docker's own runtime. A
// Unix socket is how the worker reaches into a container that has no
// network; gVisor's and Kata's runtimes do not let a container connect to
// one on the machine, and nor does Docker Desktop, whose containers run in
// a virtual machine.
const MiningEgressLabel = "mining-egress"

// miningAlgorithms are the algorithms a job may mine with, by XMRig's
// names: RandomX for Monero.
var miningAlgorithms = []string{"rx/0"}

// Bounds of a mining job: the shortest and longest a task may mine for,
// the longest a worker name and a pool password may be, and how long a
// task is given beyond its time box to start, say what it did and stop.
// Tests shorten the shortest and the grace.
var (
	miningShortest = time.Minute
	miningGrace    = 2 * time.Minute
)

const (
	miningLongest   = 24 * time.Hour
	miningMostName  = 32
	miningMostPass  = 64
	miningRelayPort = "3333"
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

// MiningOutput is what one task says it did. Every number is XMRig's own,
// read from its local API at the end of the time box: self-reported.
type MiningOutput struct {
	Index     int    `json:"index"`
	Worker    string `json:"worker"`
	Algorithm string `json:"algorithm"`
	// Pool is the one address and port the task could reach.
	Pool string `json:"pool"`
	// Seconds is the time box it mined for, and Connected how long it had
	// been connected to the pool at the end of it.
	Seconds   int   `json:"seconds"`
	Connected int64 `json:"connected_seconds"`
	// Hashrate is hashes a second over the last HashrateWindow seconds, the
	// longest of XMRig's windows (10 s, 60 s, 15 min) it had measured,
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

// miningDockerfile is the image a miner runs in: XMRig 6.26.0, built from
// its source at the release's commit, b2ca7248, fetched from GitHub and
// checked against its SHA-256. One change is made to it first: the
// donation level, which XMRig's own releases hold at a least of 1%, mining
// one minute in each hundred for XMRig's authors whatever --donate-level
// says, is made 0 by default and at least, so that a job's miner mines for
// the job alone. The build stops if that change does not take. It is
// built for the processor only, without CUDA, OpenCL, the MSR mod (which
// needs root) or the DMI reader. Beside it in the image are socat, which
// joins the miner's connection to the worker's socket, and XMRig's
// licence, the GPL 3.
const miningDockerfile = `FROM alpine:3.20 AS build
RUN apk add --no-cache build-base cmake curl hwloc-dev libuv-dev linux-headers openssl-dev
RUN curl -fsSLo /xmrig.tar.gz https://github.com/xmrig/xmrig/archive/b2ca72480c58d197e18c885d9fc1a0c8d517e60a.tar.gz \
 && echo "f8bfbd3ed3e371f51bb40e9df1b236338e5aadfa42c5bccffd59132dac7ce0a9  /xmrig.tar.gz" | sha256sum -c - \
 && mkdir /xmrig && tar -xzf /xmrig.tar.gz -C /xmrig --strip-components 1
WORKDIR /xmrig
RUN sed -i -e 's/kDefaultDonateLevel = 1;/kDefaultDonateLevel = 0;/' -e 's/kMinimumDonateLevel = 1;/kMinimumDonateLevel = 0;/' src/donate.h \
 && grep -q 'kDefaultDonateLevel = 0;' src/donate.h && grep -q 'kMinimumDonateLevel = 0;' src/donate.h
RUN mkdir build && cd build \
 && cmake .. -DCMAKE_BUILD_TYPE=Release -DWITH_OPENCL=OFF -DWITH_CUDA=OFF -DWITH_NVML=OFF -DWITH_ADL=OFF -DWITH_MSR=OFF -DWITH_DMI=OFF -DWITH_BENCHMARK=OFF \
 && nice -n 19 make -j"$(nproc)" \
 && install -m 0755 xmrig /usr/local/bin/xmrig \
 && install -D -m 0644 ../LICENSE /usr/local/share/licenses/xmrig/LICENSE \
 && install -m 0644 ../src/donate.h /usr/local/share/licenses/xmrig/donate.h
FROM alpine:3.20
RUN apk add --no-cache hwloc libstdc++ libuv openssl socat
COPY --from=build /usr/local/bin/xmrig /usr/local/bin/xmrig
COPY --from=build /usr/local/share/licenses/xmrig /usr/local/share/licenses/xmrig
RUN xmrig --version
`

// MiningImage names that image, by what it is made from.
var MiningImage = func() string {
	sum := sha256.Sum256([]byte(miningDockerfile))
	return "sisyphus-mining:" + hex.EncodeToString(sum[:6])
}()

// miningScript is what each task runs. socat listens on the container's
// own loopback and joins each connection to the worker's socket at
// /run/sisyphus-pool/pool.sock. XMRig connects to the pool by its name,
// which the container's hosts file says is that loopback address, so TLS
// is spoken to the pool's own name; it mines with no donation, with its
// local API on loopback too. At the end of the time box the script reads
// XMRig's summary from that API into /output/summary.json and stops it.
// A miner that stops before then fails the task.
const miningScript = `set -eu
socat TCP-LISTEN:` + miningRelayPort + `,bind=127.0.0.1,reuseaddr,fork UNIX-CONNECT:/run/sisyphus-pool/pool.sock &
relay=$!
set -- --url "$POOL_HOST:` + miningRelayPort + `" --user "$ADDRESS" --pass "$PASS" --rig-id "$WORKER" --algo "$ALGORITHM" \
	--donate-level 0 --no-color --print-time 60 --retry-pause 5 --randomx-mode "$RANDOMX_MODE" \
	--http-enabled --http-host 127.0.0.1 --http-port 18080
if [ "$TLS" = true ]; then set -- "$@" --tls; fi
if [ "$THREADS" != 0 ]; then set -- "$@" --threads "$THREADS"; fi
echo "mining $ALGORITHM as $WORKER for $DURATION seconds" >&2
xmrig "$@" >&2 &
miner=$!
end=$(( $(date +%s) + DURATION ))
while kill -0 "$miner" 2>/dev/null && [ "$(date +%s)" -lt "$end" ]; do sleep 1; done
if ! kill -0 "$miner" 2>/dev/null; then
	status=0
	wait "$miner" || status=$?
	kill "$relay" 2>/dev/null || true
	echo "XMRig stopped before its $DURATION seconds were up, with status $status" >&2
	exit 1
fi
wget -q -O /output/summary.json http://127.0.0.1:18080/2/summary
kill "$miner" "$relay" 2>/dev/null || true
wait "$miner" || true
echo "mined for $DURATION seconds" >&2
`

func (Mining) Name() string { return "mining" }

func (Mining) Describe() string {
	return `Mines a coin to the requester's own wallet with XMRig, built from source with no donation, on the processor: each task connects to the mining pool the job names and submits shares under the payout address it gives, for the time box, and the pool pays that address. Nothing secret is taken and no coins pass through the pool of computers. Only workers whose owners opted in to mining take it, and each task can reach the mining pool's one address and nothing else; a pool on a private, loopback, link-local or CGNAT address is refused. Parameters: {"algorithm": "<rx/0 (RandomX, Monero), the default>", "pool": "<stratum+tcp://host:port, or stratum+ssl://host:port for TLS>", "address": "<the public Monero address the pool pays>", "worker": "<optional name prefix, letters, digits, dots, dashes and underscores, default sisyphus; each task adds -<job>-<n>>", "pass": "<optional pool password field, default x; never a secret>", "duration": "<how long each task mines, 1m to 24h, as 90m or 2h>", "tasks": <optional number of tasks>}. Result: {"self_reported": true, "algorithm": "rx/0", "tasks": [{"index": <n>, "worker": "<name>", "algorithm": "rx/0", "pool": "<address:port reached>", "seconds": <time box>, "connected_seconds": <n>, "hashrate": <hashes a second, or null>, "hashrate_window": <seconds it is over>, "hashrate_highest": <n or null>, "accepted": <shares>, "rejected": <shares>}], "hashrate": <sum>, "accepted": <n>, "rejected": <n>}. The numbers are each miner's own; the pool's statistics for the address are the real check.`
}

// OptIn says a worker takes mining only when its owner says so.
func (Mining) OptIn() bool { return true }

// Groups are mining's own groups, by which an owner may allow it.
func (Mining) Groups() []string { return []string{GroupMining, GroupMiningCPU, GroupMiningRandomX} }

// Varies says two tasks never mine the same: no result can be checked by
// running it again.
func (Mining) Varies() bool { return true }

// Needs says a mining task needs a worker that can hold it to its pool.
func (Mining) Needs([]byte) []string { return []string{MiningEgressLabel} }

// Offers is holding a miner to its pool, where this worker can.
func (m Mining) Offers(context.Context) []string {
	if m.canHold() {
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

// env is the settings as a task's environment.
func (t miningTask) env(confined Confinement) map[string]string {
	host := t.Host
	if _, err := netip.ParseAddr(host); err == nil {
		host = "127.0.0.1"
	}
	// RandomX's fast mode holds a 2 GiB dataset in memory; with less than
	// 3 GiB allowed a task, it runs in light mode, with a 256 MiB cache.
	mode := "auto"
	if confined.MemoryMB > 0 && confined.MemoryMB < 3072 {
		mode = "light"
	}
	return map[string]string{
		"ALGORITHM": t.Algorithm, "POOL_HOST": host, "TLS": strconv.FormatBool(t.Scheme == "stratum+ssl"),
		"ADDRESS": t.Address, "PASS": t.Pass, "WORKER": t.Worker, "DURATION": strconv.Itoa(t.Seconds),
		"RANDOMX_MODE": mode, "THREADS": strconv.Itoa(int(math.Ceil(confined.CPUs))),
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
	box := time.Duration(task.Seconds)*time.Second + miningGrace
	if m.Confined.Timeout > 0 && box > m.Confined.Timeout {
		return nil, fmt.Errorf("the task would mine for %s, and this worker's owner allows a task %s", time.Duration(task.Seconds)*time.Second, m.Confined.Timeout)
	}
	engine := m.Engine
	if engine == nil {
		engine = docker
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
	if err := haveImage(ctx, engine, MiningImage, miningDockerfile, "mining"); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(m.Confined.TaskDir, "sisyphus-pool-")
	if err != nil {
		return nil, fmt.Errorf("make the directory of the miner's socket: %w", err)
	}
	defer os.RemoveAll(dir)
	run, cancel := context.WithTimeout(ctx, box)
	defer cancel()
	forwarder, err := forwardPool(run, filepath.Join(dir, "pool.sock"), to, dial)
	if err != nil {
		return nil, err
	}
	report := Report(ctx)
	report.Log(fmt.Sprintf("mining %s as %s for %d seconds, reaching the pool %s at %s and nothing else", task.Algorithm, task.Worker, task.Seconds, task.Host, to))
	extra := []string{"--volume", dir + ":/run/sisyphus-pool"}
	if _, err := netip.ParseAddr(task.Host); err != nil {
		extra = append(extra, "--add-host", task.Host+":127.0.0.1")
	}
	raw, err := Container{Engine: engine, Confined: m.Confined, extra: extra}.Execute(run, blobs, mustJSON(containerTask{
		ContainerParams: ContainerParams{Image: MiningImage, Command: []string{"sh", "-c", miningScript}, Env: task.env(m.Confined)},
		Index:           task.Index, Count: 1,
	}))
	made, failed := forwarder.stop()
	report.Log(fmt.Sprintf("the miner made %d connections to its pool, and %d could not be made", made, failed))
	if err != nil {
		if run.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("the miner did not stop within %s of its time box, and was stopped", miningGrace)
		}
		return nil, err
	}
	var out ContainerOutput
	json.Unmarshal(raw, &out) // Container's own
	summary, err := readMiningSummary(ctx, blobs, out.Files["summary.json"])
	if err != nil {
		return nil, fmt.Errorf("the miner's summary: %w", err)
	}
	output, err := summary.output(task)
	if err != nil {
		return nil, fmt.Errorf("the miner's summary: %w", err)
	}
	output.Pool = to.String()
	return mustJSON(output), nil
}

// xmrigSummary is what XMRig's local API says of itself at /2/summary, as
// far as a task reads it.
type xmrigSummary struct {
	DonateLevel *int `json:"donate_level"`
	Hashrate    struct {
		Total   []*float64 `json:"total"`
		Highest *float64   `json:"highest"`
	} `json:"hashrate"`
	Connection struct {
		Algo     string `json:"algo"`
		Uptime   int64  `json:"uptime"`
		Accepted uint64 `json:"accepted"`
		Rejected uint64 `json:"rejected"`
	} `json:"connection"`
}

// xmrigWindows are the seconds XMRig's hashrates are taken over, in the
// order it gives them.
var xmrigWindows = []int{10, 60, 900}

// readMiningSummary reads the summary a task left.
func readMiningSummary(ctx context.Context, blobs Blobs, id string) (xmrigSummary, error) {
	c, err := cid.Decode(id)
	if err != nil {
		return xmrigSummary{}, errors.New("the task left none")
	}
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return xmrigSummary{}, err
	}
	defer blob.Close()
	var summary xmrigSummary
	if err := json.NewDecoder(io.LimitReader(blob, 1<<20)).Decode(&summary); err != nil {
		return xmrigSummary{}, err
	}
	return summary, nil
}

// output makes a task's output of what XMRig said, refusing what an XMRig
// built as the image builds it would not say: a donation, another
// algorithm, or a hashrate that is no number of hashes.
func (s xmrigSummary) output(task miningTask) (MiningOutput, error) {
	switch {
	case s.DonateLevel == nil || *s.DonateLevel != 0:
		return MiningOutput{}, errors.New("XMRig does not say it donates nothing, which the image's XMRig does")
	case s.Connection.Algo != "" && s.Connection.Algo != task.Algorithm:
		return MiningOutput{}, fmt.Errorf("XMRig mined %s, and the job asks for %s", s.Connection.Algo, task.Algorithm)
	case len(s.Hashrate.Total) > len(xmrigWindows):
		return MiningOutput{}, fmt.Errorf("XMRig gives %d hashrates, and has %d windows", len(s.Hashrate.Total), len(xmrigWindows))
	}
	out := MiningOutput{
		Index: task.Index, Worker: task.Worker, Algorithm: task.Algorithm, Seconds: task.Seconds,
		Connected: s.Connection.Uptime, Highest: s.Hashrate.Highest, Accepted: s.Connection.Accepted, Rejected: s.Connection.Rejected,
	}
	for i, rate := range s.Hashrate.Total {
		if rate != nil {
			out.Hashrate, out.HashrateWindow = rate, xmrigWindows[i]
		}
	}
	for _, rate := range []*float64{out.Hashrate, out.Highest} {
		if rate != nil && !(*rate >= 0 && *rate < 1e15) {
			return MiningOutput{}, fmt.Errorf("XMRig says it hashed %v times a second", *rate)
		}
	}
	if out.Connected < 0 || out.Connected > int64(task.Seconds)+int64(miningGrace/time.Second) {
		return MiningOutput{}, fmt.Errorf("XMRig says it was connected for %d seconds of a time box of %d", out.Connected, task.Seconds)
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
