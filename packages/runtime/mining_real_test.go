package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

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

// XMRig, built from source as a worker builds it, mines in a real
// container, with no network but the socket to the test's fake pool, for
// a few seconds in light mode on one thread: it logs in under the job's
// address and the task's name, finds shares and has them taken, and says
// so, with no donation. Nothing is mined for any coin.
func TestRealMinerMinesAtAFakePool(t *testing.T) {
	requireRealImages(t)
	requireDocker(t)
	asIfOnLinux(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a few minutes, the compiler at the lowest priority.
	if err := haveImage(ctx, docker, MiningImage, miningDockerfile, "mining"); err != nil {
		t.Fatal(err)
	}
	shortest := miningShortest
	miningShortest = 15 * time.Second
	t.Cleanup(func() { miningShortest = shortest })

	pool := startFakePool(t)
	port := pool.listener.Addr().(interface{ AddrPort() netip.AddrPort }).AddrPort().Port()
	m := Mining{
		Confined: Confinement{ReadOnly: true, MemoryMB: 1024, CPUs: 1},
		resolver: staticResolver{netip.MustParseAddr("127.0.0.1")},
		refused:  onlyLoopback,
	}
	store := storage.NewMemory()
	payloads, err := m.Split(ctx, store, []byte(fmt.Sprintf(`{"pool":"stratum+tcp://pool.fake.test:%d","address":%q,"worker":"rig","duration":"20s"}`, port, testMoneroAddress)), 1)
	if err != nil {
		t.Fatal(err)
	}
	said := new(heard)
	output, err := m.Execute(WithReporter(ctx, said), store, payloads[0])
	log := strings.Join(said.lines, "\n")
	t.Logf("the miner's log:\n%s", log)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := m.Aggregate(ctx, store, [][]byte{output})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("result: %s", raw)
	var result MiningResult
	json.Unmarshal(raw, &result)
	logins, shares := pool.seen()
	task := result.Tasks[0]
	if !result.SelfReported || len(logins) == 0 || logins[0]["login"] != testMoneroAddress || logins[0]["pass"] != "x" || !strings.HasPrefix(fmt.Sprint(logins[0]["rigid"]), "rig-") {
		t.Fatalf("the pool saw the logins %v, and the result is %s", logins, raw)
	}
	if shares == 0 || task.Accepted == 0 || task.Accepted > uint64(shares) || task.Rejected != 0 || task.Hashrate == nil || *task.Hashrate <= 0 || task.Pool != "127.0.0.1:"+fmt.Sprint(port) {
		t.Errorf("the pool took %d shares, and the task says %+v", shares, task)
	}
	if !strings.Contains(log, "DONATE") || !strings.Contains(log, "0%") || !strings.Contains(log, "use pool pool.fake.test:3333") {
		t.Errorf("XMRig's log does not say it donates nothing to a pool reached through the relay:\n%s", log)
	}
}
