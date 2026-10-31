package runtime

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakePool is a stratum server of the tests' own, which no coin is mined
// for: it hands every miner that logs in the same made-up RandomX job,
// with a target easy enough that a few seconds of light-mode hashing on
// one thread find shares, and takes every share it is given without
// checking it. No test ever reaches a real pool.
type fakePool struct {
	listener net.Listener

	mu     sync.Mutex
	logins []map[string]any
	shares int
}

// fakePoolJob is the job: a 76-byte blob of the shape of Monero's, its
// nonce zero, a target of difficulty about 10, and a seed hash.
var fakePoolJob = map[string]any{
	"blob":      "10" + strings.Repeat("00", 75),
	"job_id":    "fake-1",
	"target":    "99999919",
	"algo":      "rx/0",
	"height":    1,
	"seed_hash": strings.Repeat("5e", 32),
}

func startFakePool(t *testing.T) *fakePool {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePool{listener: listener}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	return p
}

func (p *fakePool) addr() string { return p.listener.Addr().String() }

func (p *fakePool) serve(conn net.Conn) {
	defer conn.Close()
	lines := bufio.NewScanner(conn)
	reply := json.NewEncoder(conn)
	for lines.Scan() {
		var call struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(lines.Bytes(), &call) != nil {
			return
		}
		var result any
		p.mu.Lock()
		switch call.Method {
		case "login":
			p.logins = append(p.logins, call.Params)
			result = map[string]any{"id": "fake-session", "job": fakePoolJob, "extensions": []string{"algo", "keepalive"}, "status": "OK"}
		case "submit":
			p.shares++
			result = map[string]any{"status": "OK"}
		default:
			result = map[string]any{"status": "KEEPALIVED"}
		}
		p.mu.Unlock()
		if reply.Encode(map[string]any{"id": call.ID, "jsonrpc": "2.0", "error": nil, "result": result}) != nil {
			return
		}
	}
}

// seen returns the logins and the count of shares the pool has had.
func (p *fakePool) seen() ([]map[string]any, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.logins...), p.shares
}
