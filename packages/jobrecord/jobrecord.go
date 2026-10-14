// Package jobrecord writes a finished job as linked data: a few small nodes
// that name each other, and the job's stored data, by CID. The CID of the
// root then names the job's whole history: what was asked, who did what,
// and what came of it. Change any of it and the root's CID changes.
//
// A record is made of these nodes, each encoded as DAG-CBOR:
//
//   - the manifest: what was submitted;
//   - one receipt for each task: who ran it, how often, and how it ended,
//     and for a job that was verified, what each worker returned and which
//     of them agreed;
//   - the result: how the job ended and what it produced;
//   - for a job with steps, the graph: the jobs that carried them out, and
//     their records;
//   - the root, which links the others.
//
// Building a record does no I/O and uses nothing but the job, so the same
// job always gives the same record. Times that are not kept across a
// restart, such as when an attempt began, are left out for that reason.
//
// A private job's record leaves out everything that was not sealed: its
// parameters, its result, what its tasks returned, the digest of what each
// worker returned for a task that was verified, and every error message,
// since a message may quote data. It says the job is private, and still
// links the job's sealed inputs and outputs. In place of each value left
// out it holds a commitment to it, which only the job's key can check; see
// Commit.
//
// A record is not signed. Signing the root with the coordinator's node key
// is future work.
package jobrecord

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	"github.com/ipld/go-ipld-prime/codec/dagjson"
	"github.com/ipld/go-ipld-prime/datamodel"
	"github.com/ipld/go-ipld-prime/fluent"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/ipld/go-ipld-prime/traversal"
	"github.com/multiformats/go-multihash"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

const (
	// Version is the version of the record's layout, which every root
	// carries. A change that alters what a field means gets a new one.
	Version = 1
	// Kind is what every root says it is.
	Kind = "sisyphus-job-record"
	// InlineLimit is the most bytes of parameters, of a result or of a
	// task's output that a node holds itself. Anything longer is named by
	// the CID it has as a stored blob. Changing it changes the CID of
	// every record with a value between the old limit and the new.
	InlineLimit = 16 << 10
)

// Step is a job that carried out a step of another.
type Step struct {
	// Name is the step's name, and Job the ID of the job.
	Name, Job string
	// Record is the CID of that job's record.
	Record string
}

// Node is one node of a record: its DAG-CBOR encoding, and the CID of
// that.
type Node struct {
	CID  cid.Cid
	Data []byte
}

// Blob is a value too long to sit in a node, which the record links to.
type Blob struct {
	CID  cid.Cid
	Data []byte
}

// Record is a job's record, ready to be stored.
type Record struct {
	Root cid.Cid
	// Nodes are the record's nodes: the root first, then the nodes it
	// links to, in the order Read returns them.
	Nodes []Node
	// Blobs are the values the nodes link to instead of holding, for
	// whoever stores the record to store beside it.
	Blobs []Blob
}

var nodeBuilder = cid.V1Builder{Codec: cid.DagCBOR, MhType: multihash.SHA2_256}

// commitmentLabel is the label under which the key for commitments is
// derived from a job's sealing key, so that the two are not the same key.
const commitmentLabel = "sisyphus job record: commitments"

// The names of a record's commitments. A name is the path from the record's
// root to where the commitment is.
const (
	paramsCommitment = "manifest/params_commitment"
	resultCommitment = "result/result_commitment"
	errorCommitment  = "result/error_commitment"
)

func outputCommitment(task int) string {
	return fmt.Sprintf("receipts/%d/output_commitment", task)
}

func attemptErrorCommitment(task, attempt int) string {
	return fmt.Sprintf("receipts/%d/attempts/%d/error_commitment", task, attempt)
}

func digestCommitment(task, result int) string {
	return fmt.Sprintf("receipts/%d/results/%d/digest_commitment", task, result)
}

// committed is a value a private job's record leaves out and commits to
// instead, with the name of its commitment.
type committed struct {
	name  string
	value []byte
}

// committedValues returns the values a private job's record commits to, in
// the order the record has them.
func committedValues(job *jobmodel.Job) []committed {
	values := []committed{{paramsCommitment, job.Params}}
	for i, task := range job.Tasks {
		values = append(values, committed{outputCommitment(i), task.Output})
		for n, attempt := range task.History {
			values = append(values, committed{attemptErrorCommitment(i, n), []byte(attempt.Err)})
		}
		for n, result := range task.Results {
			values = append(values, committed{digestCommitment(i, n), []byte(result.Digest())})
		}
	}
	return append(values, committed{resultCommitment, job.Result}, committed{errorCommitment, []byte(job.Err)})
}

// Commit returns a commitment to each value the record of a private job
// leaves out, by name: the job's parameters, its result and its error, each
// task's output, the error of each attempt, and the digest of each result a
// worker returned for a task that was verified. A value that is empty is
// committed to like any other, so a commitment does not say whether there
// was one.
//
// A commitment is the HMAC-SHA-256 of the job's ID, the commitment's name
// and the value, the first two each preceded by its length as eight bytes,
// big-endian. The key is 32 bytes derived from the job's sealing key by
// HKDF-SHA-256 with no salt and the label "sisyphus job record:
// commitments". Without the sealing key a commitment says nothing of its
// value, not even whether a guess at it is right. Because the job's ID and
// the name go in, equal values in two jobs, or in two places in one job,
// give different commitments.
//
// key is the job's sealing key. A job is given its commitments when it
// ends, while it still has its key, and keeps them: Build uses them, and
// never the key.
func Commit(job *jobmodel.Job, key []byte) map[string][]byte {
	// Deriving 32 bytes with SHA-256 cannot fail.
	derived, _ := hkdf.Key(sha256.New, key, nil, commitmentLabel, sha256.Size)
	commitments := make(map[string][]byte)
	for _, v := range committedValues(job) {
		mac := hmac.New(sha256.New, derived)
		for _, part := range []string{job.ID, v.name} {
			mac.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part))))
			mac.Write([]byte(part))
		}
		mac.Write(v.value)
		commitments[v.name] = mac.Sum(nil)
	}
	return commitments
}

// Check is whether one of a job's commitments is the one a key gives for
// the value the job has now.
type Check struct {
	// Name is the path from the record's root to the commitment.
	Name    string
	Matches bool
}

// CheckCommitments works out a job's commitments again from its values as
// they stand and the given key, and says of each, in the order the record
// has them, whether it is the one the job carries. A job that carries none,
// such as one that is not private, has nothing to check. With a key that is
// not the job's, none match.
func CheckCommitments(job *jobmodel.Job, key []byte) []Check {
	if len(job.Commitments) == 0 {
		return nil
	}
	again := Commit(job, key)
	var checks []Check
	for _, v := range committedValues(job) {
		checks = append(checks, Check{Name: v.name, Matches: hmac.Equal(again[v.name], job.Commitments[v.name])})
	}
	return checks
}

// Build returns the record of a job that is over. steps are the jobs that
// carried out its steps, if it had any, in the order they were submitted.
// name gives the CID a value would have as a stored blob.
func Build(job *jobmodel.Job, steps []Step, name func(data []byte) cid.Cid) *Record {
	b := &builder{private: job.Private, commitments: job.Commitments, name: name}

	root := fluent.MustBuildMap(basicnode.Prototype.Map, -1, func(m fluent.MapAssembler) {
		m.AssembleEntry("kind").AssignString(Kind)
		m.AssembleEntry("version").AssignInt(Version)
		m.AssembleEntry("job").AssignString(job.ID)
		m.AssembleEntry("private").AssignBool(job.Private)
		if job.Parent != "" {
			// The job this one is a step of is not over yet and has no
			// record to link, so it is named by its ID.
			m.AssembleEntry("parent").CreateMap(-1, func(m fluent.MapAssembler) {
				m.AssembleEntry("job").AssignString(job.Parent)
				m.AssembleEntry("step").AssignString(job.Step)
			})
		}
		m.AssembleEntry("manifest").AssignLink(b.add(b.manifest(job)))
		m.AssembleEntry("receipts").CreateList(-1, func(l fluent.ListAssembler) {
			for i, task := range job.Tasks {
				l.AssembleValue().AssignLink(b.add(b.receipt(i, task, job.VerifyShare > 0)))
			}
		})
		m.AssembleEntry("result").AssignLink(b.add(b.result(job)))
		if len(steps) > 0 {
			m.AssembleEntry("graph").AssignLink(b.add(graph(steps)))
		}
	})

	record := &Record{Blobs: b.blobs}
	data := encode(root)
	record.Root, _ = nodeBuilder.Sum(data) // hashing bytes in memory cannot fail
	record.Nodes = append(record.Nodes, Node{CID: record.Root, Data: data})
	// As Read returns them: in the order the root's encoding links them.
	for _, linked := range links(root) {
		record.Nodes = append(record.Nodes, Node{CID: linked, Data: b.nodes[linked]})
	}
	return record
}

type builder struct {
	private bool
	// commitments are the job's, by name; see Commit.
	commitments map[string][]byte
	name        func([]byte) cid.Cid
	nodes       map[cid.Cid][]byte
	blobs       []Blob
}

// add encodes a node, keeps it, and returns a link to it.
func (b *builder) add(node datamodel.Node) datamodel.Link {
	data := encode(node)
	c, _ := nodeBuilder.Sum(data) // hashing bytes in memory cannot fail
	if b.nodes == nil {
		b.nodes = make(map[cid.Cid][]byte)
	}
	b.nodes[c] = data
	return cidlink.Link{Cid: c}
}

// value puts bytes that are not secret in a node under key: themselves if
// they are few, a link to them as a blob if they are many, and nothing at
// all if there are none. For a private job it puts the commitment of the
// given name instead; see commit.
func (b *builder) value(m fluent.MapAssembler, key string, data []byte, commitment string) {
	switch {
	case b.private:
		b.commit(m, key, commitment)
	case len(data) == 0:
	case len(data) <= InlineLimit:
		m.AssembleEntry(key).AssignBytes(data)
	default:
		c := b.name(data)
		b.blobs = append(b.blobs, Blob{CID: c, Data: data})
		m.AssembleEntry(key).AssignLink(cidlink.Link{Cid: c})
	}
}

// message puts an error message in a node under key, unless there is none.
// For a private job it puts the commitment of the given name instead.
func (b *builder) message(m fluent.MapAssembler, key, text, commitment string) {
	switch {
	case b.private:
		b.commit(m, key, commitment)
	case text != "":
		m.AssembleEntry(key).AssignString(text)
	}
}

// commit puts the commitment of the given name in a node, under key with
// "_commitment" after it. A private job that ended before commitments were
// made has none, and its record is as it was then.
func (b *builder) commit(m fluent.MapAssembler, key, name string) {
	if commitment, has := b.commitments[name]; has {
		m.AssembleEntry(key + "_commitment").AssignBytes(commitment)
	}
}

func (b *builder) manifest(job *jobmodel.Job) datamodel.Node {
	return fluent.MustBuildMap(basicnode.Prototype.Map, -1, func(m fluent.MapAssembler) {
		m.AssembleEntry("workload").AssignString(job.Workload)
		b.value(m, "params", job.Params, paramsCommitment)
		mode := "distributed"
		if job.Mode == jobmodel.FullWorker {
			mode = "full-worker"
		}
		m.AssembleEntry("mode").AssignString(mode)
		m.AssembleEntry("requirements").CreateMap(-1, func(m fluent.MapAssembler) {
			m.AssembleEntry("max_tasks").AssignInt(int64(job.MaxTasks))
			m.AssembleEntry("task_timeout_seconds").AssignInt(int64(job.TaskTimeout / time.Second))
			m.AssembleEntry("min_memory_bytes").AssignInt(int64(job.MinMemory))
			m.AssembleEntry("min_gpus").AssignInt(int64(job.MinGPUs))
			// Only where it was asked for, so that the record of a job
			// that was not verified is what it always was.
			if job.Verify >= 2 {
				m.AssembleEntry("verify").AssignInt(int64(job.Verify))
			}
			// And only where not every task was to be verified.
			if job.VerifyShare > 0 {
				m.AssembleEntry("verify_share").AssignFloat(job.VerifyShare)
			}
			// And the share the coordinator was to run again itself.
			if job.AuditShare > 0 {
				m.AssembleEntry("audit_share").AssignFloat(job.AuditShare)
			}
		})
		blobs(m, "inputs", job.InputBlobs())
		if job.Submitter != "" {
			m.AssembleEntry("submitter").AssignString(job.Submitter)
		}
		moment(m, "created_at", job.CreatedAt)
	})
}

// receipt is the node for the task at index in the job's tasks.
func (b *builder) receipt(index int, task *jobmodel.Task, spot bool) datamodel.Node {
	return fluent.MustBuildMap(basicnode.Prototype.Map, -1, func(m fluent.MapAssembler) {
		m.AssembleEntry("task").AssignInt(int64(task.Index))
		m.AssembleEntry("state").AssignString(task.State.String())
		m.AssembleEntry("failures").AssignInt(int64(task.Failures))
		b.value(m, "output", task.Output, outputCommitment(index))
		// Every time the task was handed to a worker, in order. The last
		// is the one that settled it.
		m.AssembleEntry("attempts").CreateList(-1, func(l fluent.ListAssembler) {
			for n, attempt := range task.History {
				l.AssembleValue().CreateMap(-1, func(m fluent.MapAssembler) {
					m.AssembleEntry("attempt").AssignInt(int64(attempt.Number))
					m.AssembleEntry("node").AssignString(attempt.NodeID)
					m.AssembleEntry("name").AssignString(attempt.NodeName)
					m.AssembleEntry("state").AssignString(attempt.State.String())
					b.message(m, "error", attempt.Err, attemptErrorCommitment(index, n))
				})
			}
		})
		if len(task.Results) == 0 {
			return
		}
		// How many of them had to be the same, in a job that did not ask
		// it of every task: one for a task that was run once.
		if spot {
			m.AssembleEntry("verify").AssignInt(int64(task.Verify))
		}
		// What each worker returned for a task that was verified, by a
		// digest that is the same for results that are the same, and
		// whether it is one of those the task was settled by. The digest
		// of a private job's result is left out like its output, which it
		// is a hash of: someone who guessed the output could tell from it
		// that they were right.
		agreed := task.Agreed()
		m.AssembleEntry("results").CreateList(-1, func(l fluent.ListAssembler) {
			for n, r := range task.Results {
				l.AssembleValue().CreateMap(-1, func(m fluent.MapAssembler) {
					m.AssembleEntry("attempt").AssignInt(int64(r.Attempt))
					m.AssembleEntry("node").AssignString(r.NodeID)
					m.AssembleEntry("name").AssignString(r.NodeName)
					b.message(m, "digest", r.Digest(), digestCommitment(index, n))
					m.AssembleEntry("agreed").AssignBool(len(agreed) > 0 && r.Digest() == agreed[0].Digest())
				})
			}
		})
	})
}

func (b *builder) result(job *jobmodel.Job) datamodel.Node {
	return fluent.MustBuildMap(basicnode.Prototype.Map, -1, func(m fluent.MapAssembler) {
		m.AssembleEntry("state").AssignString(job.State.String())
		b.value(m, "result", job.Result, resultCommitment)
		b.message(m, "error", job.Err, errorCommitment)
		blobs(m, "outputs", job.OutputBlobs())
		// What only passed between the job's tasks. The store lets these
		// go when the job ends; the record still names them.
		blobs(m, "intermediate", job.IntermediateBlobs())
		moment(m, "finished_at", job.FinishedAt)
	})
}

// graph is the node that says which jobs carried out a job's steps.
func graph(steps []Step) datamodel.Node {
	return fluent.MustBuildMap(basicnode.Prototype.Map, -1, func(m fluent.MapAssembler) {
		m.AssembleEntry("steps").CreateList(-1, func(l fluent.ListAssembler) {
			for _, step := range steps {
				l.AssembleValue().CreateMap(-1, func(m fluent.MapAssembler) {
					m.AssembleEntry("step").AssignString(step.Name)
					m.AssembleEntry("job").AssignString(step.Job)
					if c, err := cid.Decode(step.Record); err == nil {
						m.AssembleEntry("record").AssignLink(cidlink.Link{Cid: c})
					}
				})
			}
		})
	})
}

// blobs puts links to stored blobs in a node under key, in order of CID.
// A name that is not a CID is left out: they come from workers, which may
// send anything.
func blobs(m fluent.MapAssembler, key string, cids []string) {
	sorted := append([]string(nil), cids...)
	sort.Strings(sorted)
	m.AssembleEntry(key).CreateList(-1, func(l fluent.ListAssembler) {
		for _, s := range sorted {
			if c, err := cid.Decode(s); err == nil {
				l.AssembleValue().AssignLink(cidlink.Link{Cid: c})
			}
		}
	})
}

// moment puts a time in a node under key, to the nanosecond and in UTC, or
// nothing if there is no time to put.
func moment(m fluent.MapAssembler, key string, t time.Time) {
	if !t.IsZero() {
		m.AssembleEntry(key).AssignString(t.UTC().Format(time.RFC3339Nano))
	}
}

// encode returns a node's DAG-CBOR encoding, which is the same however the
// node was put together: map keys are written in one fixed order.
func encode(node datamodel.Node) []byte {
	var data bytes.Buffer
	dagcbor.Encode(node, &data) // a node built here always encodes, and a buffer takes it
	return data.Bytes()
}

// links returns the nodes a node links to, in the order its encoding does.
func links(node datamodel.Node) []cid.Cid {
	decoded, _ := decode(encode(node))
	return nodeLinks(decoded)
}

func decode(data []byte) (datamodel.Node, error) {
	builder := basicnode.Prototype.Any.NewBuilder()
	if err := dagcbor.Decode(builder, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return builder.Build(), nil
}

// nodeLinks returns the links in a node that are to other nodes rather
// than to blobs, each once, in the order they appear.
func nodeLinks(node datamodel.Node) []cid.Cid {
	found, _ := traversal.SelectLinks(node) // a decoded node always lists its links
	var nodes []cid.Cid
	seen := make(map[cid.Cid]bool)
	for _, link := range found {
		c := link.(cidlink.Link).Cid // the decoder makes no other kind
		if c.Type() == cid.DagCBOR && !seen[c] {
			seen[c] = true
			nodes = append(nodes, c)
		}
	}
	return nodes
}

// Getter is where a record is read from. A storage.Store is one.
type Getter interface {
	// GetNode returns the DAG-CBOR encoding of the node with the given
	// CID.
	GetNode(ctx context.Context, c cid.Cid) ([]byte, error)
}

// Read returns the nodes of the record whose root has the given CID: the
// root, then the nodes it links to. It does not follow the links of those,
// so the records of a job's steps are named and not fetched.
func Read(ctx context.Context, from Getter, root cid.Cid) ([]Node, error) {
	data, err := from.GetNode(ctx, root)
	if err != nil {
		return nil, err
	}
	decoded, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("node %s is not DAG-CBOR: %w", root, err)
	}
	nodes := []Node{{CID: root, Data: data}}
	for _, linked := range nodeLinks(decoded) {
		data, err := from.GetNode(ctx, linked)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, Node{CID: linked, Data: data})
	}
	return nodes, nil
}

// JSON returns the node in DAG-JSON, for reading: a link is {"/": "<cid>"}
// and bytes are {"/": {"bytes": "<base64>"}}. A node that is not DAG-CBOR
// has none, and gives "null".
func (n Node) JSON() string {
	node, err := decode(n.Data)
	if err != nil {
		return "null"
	}
	var text bytes.Buffer
	dagjson.Encode(node, &text) // a decoded node always encodes, and a buffer takes it
	return text.String()
}
