package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/names"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// Two files, by the IDs their content gives them.
const (
	fileA = "bafkreih5u7lamz7ygaeqakgswmw5r5d5qytmkx4sndacbh4uvd3p5j4lqu"
	fileB = "bafkreig65te5vcaby4yjbatp2xsk2ylupl3m5eozxezvxu7v3i2x3o4lgm"
)

// kept is a node's main address that answers as it is set to, and fails at
// whatever call is named in failing.
type kept struct {
	failing string

	pins     []*pb.Pin
	replicas *pb.ReplicasResponse
	// cluster is how the pool's cluster stands; a node with none refuses.
	cluster  *pb.ClusterStatusResponse
	restored *pb.RestoreResponse
	record   *pb.JobRecord
	// held is the record the node holds for a name, and published the one
	// it was last handed.
	held, published []byte

	// What the node was last asked, and with what said of who is asking.
	pinned    *pb.PinBlobRequest
	asked     *pb.GetJobRecordRequest
	copiesOf  string
	shownWith []string
}

func (k *kept) fail(call string) error {
	if k.failing == call {
		return errDown
	}
	return nil
}

type keptBlobs struct {
	pb.BlobServiceClient
	*kept
}

func (k keptBlobs) ListPins(ctx context.Context, _ *pb.ListPinsRequest, _ ...grpc.CallOption) (*pb.ListPinsResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	k.shownWith = md.Get("authorization")
	return &pb.ListPinsResponse{Pins: k.pins}, k.fail("ListPins")
}

func (k keptBlobs) Pin(_ context.Context, req *pb.PinBlobRequest, _ ...grpc.CallOption) (*pb.PinBlobResponse, error) {
	k.pinned = req
	return &pb.PinBlobResponse{}, k.fail("Pin")
}

func (k keptBlobs) Unpin(context.Context, *pb.UnpinBlobRequest, ...grpc.CallOption) (*pb.UnpinBlobResponse, error) {
	return &pb.UnpinBlobResponse{}, k.fail("Unpin")
}

func (k keptBlobs) CollectGarbage(context.Context, *pb.CollectGarbageRequest, ...grpc.CallOption) (*pb.CollectGarbageResponse, error) {
	return &pb.CollectGarbageResponse{ExpiredPins: 2, BlocksRemoved: 7, BytesFreed: 4096}, k.fail("CollectGarbage")
}

func (k keptBlobs) Replicas(_ context.Context, req *pb.ReplicasRequest, _ ...grpc.CallOption) (*pb.ReplicasResponse, error) {
	k.copiesOf = req.GetCid()
	return k.replicas, k.fail("Replicas")
}

func (k keptBlobs) Restore(context.Context, *pb.RestoreRequest, ...grpc.CallOption) (*pb.RestoreResponse, error) {
	return k.restored, k.fail("Restore")
}

type keptNames struct {
	pb.NameServiceClient
	*kept
}

func (k keptNames) Resolve(context.Context, *pb.ResolveNameRequest, ...grpc.CallOption) (*pb.ResolveNameResponse, error) {
	if err := k.fail("Resolve"); err != nil {
		return nil, err
	}
	if k.held == nil {
		return nil, status.Error(codes.NotFound, "nothing is published under that name")
	}
	return &pb.ResolveNameResponse{Record: k.held}, nil
}

func (k keptNames) Publish(_ context.Context, req *pb.PublishNameRequest, _ ...grpc.CallOption) (*pb.PublishNameResponse, error) {
	k.published = req.GetRecord()
	return &pb.PublishNameResponse{}, k.fail("Publish")
}

type keptJobs struct {
	pb.NodeServiceClient
	*kept
}

func (k keptJobs) GetJobRecord(_ context.Context, req *pb.GetJobRecordRequest, _ ...grpc.CallOption) (*pb.JobRecord, error) {
	k.asked = req
	return k.record, k.fail("GetJobRecord")
}

type keptPool struct {
	pb.PoolServiceClient
	*kept
}

func (k keptPool) ClusterStatus(context.Context, *pb.ClusterStatusRequest, ...grpc.CallOption) (*pb.ClusterStatusResponse, error) {
	if err := k.fail("ClusterStatus"); err != nil {
		return nil, err
	}
	if k.cluster == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node does not run an IPFS Cluster peer")
	}
	return k.cluster, nil
}

// reaching returns the main address of k as a server is given it.
func reaching(t *testing.T, k *kept) *Main {
	t.Helper()
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	if k.replicas == nil {
		k.replicas = &pb.ReplicasResponse{}
	}
	return &Main{
		Blobs: keptBlobs{kept: k}, Names: keptNames{kept: k}, Jobs: keptJobs{kept: k}, Pool: keptPool{kept: k}, Identity: ident,
		SealingKey: func() ([]byte, error) { return []byte("the node's sealing key"), k.fail("SealingKey") },
	}
}

// told is what a tool answers, as the agent reads it, or the words of its
// failure after "failed: ".
func told(v any, err error) string {
	result, _, err := shown(v, err)
	if err != nil {
		return "failed: " + err.Error()
	}
	return result.Content[0].(*mcp.TextContent).Text
}

// has reports, as a test's error, whichever of the wanted pieces a tool's
// answer lacks.
func has(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, piece := range want {
		if !strings.Contains(got, piece) {
			t.Errorf("%s: no %q in %s", what, piece, got)
		}
	}
}

func TestToolsForWhatTheNodeKeepsSayWhatTheNodeSaidWhenItFailsThem(t *testing.T) {
	ctx := context.Background()
	for failing, call := range map[string]func(*Main) (any, error){
		"ListPins":      func(m *Main) (any, error) { return listPins(ctx, m, pinsArgs{}) },
		"Replicas":      func(m *Main) (any, error) { return storageStatus(ctx, m, copiesArgs{}) },
		"ClusterStatus": func(m *Main) (any, error) { return storageStatus(ctx, m, copiesArgs{}) },
		"GetJobRecord":  func(m *Main) (any, error) { return jobRecord(ctx, m, recordArgs{JobID: "job-1"}) },
		"SealingKey": func(m *Main) (any, error) {
			return jobRecord(ctx, m, recordArgs{JobID: "job-1", CheckCommitments: true})
		},
		"Resolve":        func(m *Main) (any, error) { return resolveName(ctx, m, nameArgs{}) },
		"Publish":        func(m *Main) (any, error) { return publishName(ctx, m, publishArgs{CID: fileA}) },
		"Pin":            func(m *Main) (any, error) { return pinFile(ctx, m, pinArgs{CID: fileA}) },
		"Unpin":          func(m *Main) (any, error) { return unpinFile(ctx, m, cidArgs{CID: fileA}) },
		"CollectGarbage": func(m *Main) (any, error) { return collectGarbage(ctx, m, none{}) },
		"Restore":        func(m *Main) (any, error) { return restoreFiles(ctx, m, none{}) },
	} {
		if got := told(call(reaching(t, &kept{failing: failing}))); !strings.HasPrefix(got, "failed: ") || !strings.Contains(got, "the node is down") {
			t.Errorf("with %s failing: %s", failing, got)
		}
	}
	// A name's last record is asked for before a new one is signed.
	if got := told(publishName(ctx, reaching(t, &kept{failing: "Resolve"}), publishArgs{CID: fileA})); got != "failed: the node is down" {
		t.Errorf("publishing when the last record cannot be had: %s", got)
	}
	// What is no content ID, or no length of time, never reaches the node.
	m := reaching(t, &kept{})
	for what, got := range map[string]string{
		"pins on no file":          told(listPins(ctx, m, pinsArgs{CID: "nonsense"})),
		"copies of no file":        told(storageStatus(ctx, m, copiesArgs{CID: "nonsense"})),
		"a name for no file":       told(publishName(ctx, m, publishArgs{CID: "nonsense"})),
		"a name for less than no":  told(publishName(ctx, m, publishArgs{CID: fileA, LifetimeHours: -1})),
		"a name for an instant":    told(publishName(ctx, m, publishArgs{CID: fileA, LifetimeHours: 1e-9})),
		"a pin for less than none": told(pinFile(ctx, m, pinArgs{CID: fileA, TTLHours: -2})),
		"a pin for an age":         told(pinFile(ctx, m, pinArgs{CID: fileA, TTLHours: 1e9})),
		"a pin for an instant":     told(pinFile(ctx, m, pinArgs{CID: fileA, TTLHours: 1e-9})),
		"no name at all":           told(resolveName(ctx, m, nameArgs{Name: "not a name"})),
	} {
		if !strings.HasPrefix(got, "failed: ") || strings.Contains(got, "the node is down") {
			t.Errorf("%s: %s", what, got)
		}
	}
}

func TestPinsAreListedWithWhoseTheyAreAndUntilWhen(t *testing.T) {
	ctx := context.Background()
	until := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	k := &kept{pins: []*pb.Pin{
		{Cid: fileA, Owner: "user"},
		{Cid: fileA, Owner: "job:job-1", ExpiresAt: timestamppb.New(until)},
		{Cid: fileB, Owner: "recent", ExpiresAt: timestamppb.New(until)},
	}}
	m := reaching(t, k)
	has(t, "every pin", told(listPins(ctx, m, pinsArgs{})), `"count":3`, `{"cid":"`+fileA+`","held_for":"user","until":"it is released"}`, `"held_for":"job:job-1","until":"2026-10-10T12:00:00Z"`, `"held_for":"recent"`)
	one := told(listPins(ctx, m, pinsArgs{CID: fileB}))
	if has(t, "the pins on one file", one, `"count":1`, `"held_for":"recent"`); strings.Contains(one, "user") || strings.Contains(one, "note") {
		t.Errorf("the pins on one file: %s", one)
	}
	// A file may be named as it was before content IDs were written as now.
	const old = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
	has(t, "a file nothing pins", told(listPins(ctx, m, pinsArgs{CID: old})), `"count":0`, `"pins":[]`, "nothing pins this file")

	for i := range mostPins + 5 {
		k.pins = append(k.pins, &pb.Pin{Cid: fileB, Owner: fmt.Sprintf("job:%d", i)})
	}
	many := told(listPins(ctx, m, pinsArgs{}))
	if has(t, "more pins than are listed", many, fmt.Sprintf(`"count":%d`, mostPins+8), fmt.Sprintf("only the first %d of %d pins", mostPins, mostPins+8)); strings.Count(many, `"cid"`) != mostPins {
		t.Errorf("%d pins were listed", strings.Count(many, `"cid"`))
	}
}

func TestAPoolThatKeepsNoCopiesSaysSo(t *testing.T) {
	ctx := context.Background()
	k := &kept{}
	alone := told(storageStatus(ctx, reaching(t, k), copiesArgs{CID: fileA}))
	if has(t, "a node with no followers and no cluster", alone, `"cid":"`+fileA+`"`, `"kept_by":"this node alone"`, "without --replicas and without --cluster"); strings.Contains(alone, "after_a_lost_store") || k.copiesOf != fileA {
		t.Errorf("a node alone: %s, asked of %q", alone, k.copiesOf)
	}
	// What it has taken back from a store it lost is told however it keeps copies.
	k.replicas = &pb.ReplicasResponse{Restored: 3}
	back := told(storageStatus(ctx, reaching(t, k), copiesArgs{}))
	if has(t, "a node that took its store back", back, `"restored":3`, `"restoring":false`); strings.Contains(back, "restore_files") {
		t.Errorf("with nothing unsigned: %s", back)
	}
	k.replicas = &pb.ReplicasResponse{Restoring: true, FromEarlierStore: 5, UnsignedFromEarlierStore: 2}
	has(t, "a node with more to take back", told(storageStatus(ctx, reaching(t, k), copiesArgs{})),
		`"restoring":true`, `"held_by_followers_from_an_earlier_store":5`, `"of_those_unsigned":2`, "restore_files")
}

func TestCopiesOnFollowersAreCountedAgainstThoseWanted(t *testing.T) {
	ctx := context.Background()
	k := &kept{replicas: &pb.ReplicasResponse{Wanted: 2, Followers: []string{"follower-1"}, Settling: true, FromEarlierStore: 1, ChallengesFailed: 2, Blobs: []*pb.ReplicatedBlob{
		{Cid: fileA, Holders: []string{"follower-1"}, Shown: []string{"follower-1"}},
		{Cid: fileB},
	}}}
	got := told(storageStatus(ctx, reaching(t, k), copiesArgs{}))
	has(t, "too few followers", got, `"kept_by":"followers"`, `"copies_wanted":2`, `"followers_connected":1`, `"too_few_followers_by":1`, `"settling":"the node started`,
		`{"cid":"`+fileA+`","copies":1,"copies_shown":1,"held_by":["follower-1"]}`, `{"cid":"`+fileB+`","copies":0,"copies_shown":0,"held_by":[]}`, `"files_pinned":2`, `"files_with_too_few_copies":2`, `"of_those_unsigned":0`,
		`"copies_not_shown":"2 time(s) since the node started, a follower asked to show it held a file it said it held did not`)
	if strings.Contains(got, `"cluster"`) || strings.Contains(got, `"note"`) || strings.Contains(got, "followers_not_named") {
		t.Errorf("followers alone: %s", got)
	}

	// More followers and files than an answer has room to name.
	crowd := &pb.ReplicasResponse{Wanted: 1}
	for i := range mostNamed + 3 {
		crowd.Followers = append(crowd.Followers, fmt.Sprintf("follower-%d", i))
	}
	for range mostCopies + 4 {
		crowd.Blobs = append(crowd.Blobs, &pb.ReplicatedBlob{Cid: fileA, Holders: crowd.Followers})
	}
	k.replicas = crowd
	got = told(storageStatus(ctx, reaching(t, k), copiesArgs{}))
	has(t, "a crowd of followers", got, `"followers_not_named":3`, fmt.Sprintf(`"followers_connected":%d`, mostNamed+3), `"files_with_too_few_copies":0`,
		fmt.Sprintf(`"copies":%d`, mostNamed+3), fmt.Sprintf("only the first %d of %d files", mostCopies, mostCopies+4))
	if strings.Count(got, `"cid"`) != mostCopies || strings.Contains(got, "too_few_followers") || strings.Contains(got, "settling") || strings.Contains(got, fmt.Sprintf(`"follower-%d"`, mostNamed)) {
		t.Errorf("a crowd of followers: %d files listed in %s", strings.Count(got, `"cid"`), got)
	}
}

func TestAClustersMembersAndWhichHoldEachPin(t *testing.T) {
	ctx := context.Background()
	k := &kept{replicas: &pb.ReplicasResponse{Wanted: 0}, cluster: &pb.ClusterStatusResponse{Replicas: 2,
		Members: []*pb.ClusterMember{{NodeId: "node-1", Name: "rig"}, {NodeId: "node-2", Name: "laptop", Error: "context deadline exceeded"}},
		Pins: []*pb.ClusterPin{
			{Cid: fileA, Copies: []*pb.ClusterCopy{{NodeId: "node-1", Name: "rig", Status: "pinned"}, {NodeId: "node-2", Status: "pinned"}}},
			{Cid: fileB, Copies: []*pb.ClusterCopy{{NodeId: "node-1", Name: "rig", Status: "pinned"}, {NodeId: "node-2", Name: "laptop", Status: "pin_error", Error: "no route"}, {NodeId: "node-3", Name: "desk", Status: "pinning"}}},
		}}}
	got := told(storageStatus(ctx, reaching(t, k), copiesArgs{}))
	has(t, "a cluster", got, `"kept_by":"cluster"`, `"copies_wanted":2`, `"members_heard_from":2`, `{"answering":true,"name":"rig","node_id":"node-1"}`, `"answering":false,"error":"context deadline exceeded"`,
		`{"cid":"`+fileA+`","copies":2,"held_by":["rig","node-2"]}`, `"copies":1,"held_by":["rig"],"waiting_for":["laptop (pin_error: no route)","desk (pinning)"]`, `"files_pinned":2`, `"files_with_too_few_copies":1`)
	if strings.Contains(got, `"followers"`) || strings.Contains(got, `"note"`) {
		t.Errorf("a cluster alone: %s", got)
	}
	one := told(storageStatus(ctx, reaching(t, k), copiesArgs{CID: fileA}))
	if has(t, "one file on a cluster", one, `"files_pinned":1`, `"files_with_too_few_copies":0`); strings.Contains(one, fileB) {
		t.Errorf("one file on a cluster: %s", one)
	}

	for i := range mostNamed + 2 {
		k.cluster.Members = append(k.cluster.Members, &pb.ClusterMember{NodeId: fmt.Sprintf("more-%d", i)})
	}
	for range mostCopies + 1 {
		k.cluster.Pins = append(k.cluster.Pins, &pb.ClusterPin{Cid: fileB})
	}
	got = told(storageStatus(ctx, reaching(t, k), copiesArgs{}))
	has(t, "a large cluster", got, fmt.Sprintf(`"members_heard_from":%d`, mostNamed+4), fmt.Sprintf("only the first %d of %d files", mostCopies, mostCopies+3))
	if strings.Count(got, `"node_id"`) != mostNamed || strings.Count(got, `"cid"`) != mostCopies {
		t.Errorf("a large cluster names %d members and %d files", strings.Count(got, `"node_id"`), strings.Count(got, `"cid"`))
	}
}

func TestAJobsRecordIsShownAndCheckedAsAsked(t *testing.T) {
	ctx := context.Background()
	k := &kept{record: &pb.JobRecord{JobId: "job-1", RootCid: "root-cid", Matches: true, Nodes: []*pb.RecordNode{
		{Cid: "root-cid", Json: `{"manifest":{"/":"manifest-cid"},"state":"succeeded"}`},
		{Cid: "manifest-cid", Json: `not json at all`},
	}}}
	m := reaching(t, k)
	plain := told(jobRecord(ctx, m, recordArgs{JobID: "job-1"}))
	if has(t, "a record", plain, `"job_id":"job-1"`, `"record_cid":"root-cid"`, `"root-cid":{"manifest":{"/":"manifest-cid"},"state":"succeeded"}`, `"manifest-cid":"not json at all"`); strings.Contains(plain, "verified") || strings.Contains(plain, "commitments") || strings.Contains(plain, "note") || k.asked.GetVerify() || k.asked.GetKey() != nil {
		t.Errorf("a record not asked to be checked: %s, asked with %v", plain, k.asked)
	}
	if got := told(jobRecord(ctx, m, recordArgs{JobID: "job-1", Verify: true})); !strings.Contains(got, `"verified":true`) || !k.asked.GetVerify() || k.asked.GetKey() != nil {
		t.Errorf("a record verified: %s, asked with %v", got, k.asked)
	}
	// A key asks for the record to be verified too, and with none of a
	// private job's commitments in it there is nothing more to check.
	if got := told(jobRecord(ctx, m, recordArgs{JobID: "job-1", CheckCommitments: true})); !strings.Contains(got, "none to check") || !strings.Contains(got, "verified") || !k.asked.GetVerify() || string(k.asked.GetKey()) != "the node's sealing key" {
		t.Errorf("a record with no commitments, checked: %s, asked with %v", got, k.asked)
	}

	k.record.Matches, k.record.RecomputedCid = false, "another-cid"
	if got := told(jobRecord(ctx, m, recordArgs{JobID: "job-1", Verify: true})); got != "failed: job job-1 does not match its record root-cid: worked out again from the job as the node holds it now, the record would be another-cid" {
		t.Errorf("a record that is not the job's: %s", got)
	}
	// Not asked to check, it is shown as it is.
	if got := told(jobRecord(ctx, m, recordArgs{JobID: "job-1"})); strings.HasPrefix(got, "failed") {
		t.Errorf("a record not checked: %s", got)
	}

	// A private job's record holds commitments, which only a key checks.
	k.record = &pb.JobRecord{JobId: "job-2", RootCid: "root-cid", Matches: true, Nodes: []*pb.RecordNode{
		{Cid: "root-cid", Json: `{"params_commitment":{"/":{"bytes":"c2VhbGVk"}}}`},
		{Cid: "long-cid", Json: `"` + strings.Repeat("x", maxResult) + `"`},
	}}
	sealed := told(jobRecord(ctx, m, recordArgs{JobID: "job-2", Verify: true}))
	has(t, "a private job's record", sealed, "not checked: this is a private job's record", "1 of its 2 nodes are left out", `sisyphusd job record job-2`, `"root-cid":{"params_commitment"`)
	if strings.Contains(sealed, "long-cid") {
		t.Errorf("a node too long to show was shown: %.300s", sealed)
	}
	k.record.Commitments = []*pb.CommitmentCheck{{Name: "manifest/params_commitment", Matches: true}, {Name: "result/result_commitment", Matches: true}}
	has(t, "commitments that match", told(jobRecord(ctx, m, recordArgs{JobID: "job-2", CheckCommitments: true})), "checked: this node's key and the values the node holds give all 2", "verified")
	k.record.Commitments[1].Matches = false
	if got := told(jobRecord(ctx, m, recordArgs{JobID: "job-2", CheckCommitments: true})); !strings.HasPrefix(got, "failed: 1 of the 2 commitments in the record of job job-2 do not match (result/result_commitment, and 0 more)") || !strings.Contains(got, "is root-cid") {
		t.Errorf("a commitment that does not match: %s", got)
	}
}

func TestANameIsPublishedOverTheLastAndResolvedOnlyIfItsNodeSignedIt(t *testing.T) {
	ctx := context.Background()
	k := &kept{}
	m := reaching(t, k)
	id := m.Identity.ID()

	// Nothing published yet: the first record is numbered nought.
	first := told(publishName(ctx, m, publishArgs{CID: fileA}))
	has(t, "a first record", first, `"name":"`+id+`"`, `"cid":"`+fileA+`"`, `"sequence":0`, `"good_until":"20`, fmt.Sprintf(`"ask_again_after_seconds":%d`, int(names.DefaultTTL/time.Second)))
	record, err := names.Check(id, k.published, time.Now())
	if err != nil || record.Value.String() != fileA || time.Until(record.Expires) < names.DefaultLifetime-time.Minute {
		t.Fatalf("the record handed to the node: %+v, %v", record, err)
	}

	// The node holds it, so the next is numbered one more, and it is what
	// the name resolves to, asked by name or as this node's own.
	k.held = k.published
	has(t, "a second record", told(publishName(ctx, m, publishArgs{CID: fileB, LifetimeHours: 0.5, TTLSeconds: 60})), `"cid":"`+fileB+`"`, `"sequence":1`, `"ask_again_after_seconds":60`)
	if record, err = names.Check(id, k.published, time.Now()); err != nil || time.Until(record.Expires) > 31*time.Minute || time.Until(record.Expires) < 29*time.Minute {
		t.Fatalf("a record good for half an hour: %+v, %v", record, err)
	}
	for _, name := range []string{"", id, "/ipns/" + id} {
		has(t, "resolving "+name, told(resolveName(ctx, m, nameArgs{Name: name})), `"name":"`+id+`"`, `"cid":"`+fileA+`"`, `"sequence":0`)
	}

	// A node is not taken at its word: a record another node signed, one
	// that has run out, and what is no record are all refused.
	other := reaching(t, &kept{})
	k.held = names.Make(other.Identity, cid.MustParse(fileA), 4, time.Hour, time.Minute, time.Now()).Bytes()
	if got := told(resolveName(ctx, m, nameArgs{})); !strings.HasPrefix(got, "failed: the node gave a record for "+id+" that cannot be relied on") {
		t.Errorf("a record another node signed: %s", got)
	}
	if got := told(publishName(ctx, m, publishArgs{CID: fileA})); !strings.HasPrefix(got, "failed: the node gave, as this node's last record, one that is not") {
		t.Errorf("publishing over a record another node signed: %s", got)
	}
	k.held = names.Make(m.Identity, cid.MustParse(fileA), 4, time.Hour, time.Minute, time.Now().Add(-48*time.Hour)).Bytes()
	if got := told(resolveName(ctx, m, nameArgs{})); !strings.Contains(got, "cannot be relied on") || !strings.Contains(got, "expired") {
		t.Errorf("a record that has run out: %s", got)
	}
	// One that has run out is still the last, and is numbered after.
	has(t, "publishing over a record that has run out", told(publishName(ctx, m, publishArgs{CID: fileA})), `"sequence":5`)
	k.held = []byte("not a record")
	if got := told(resolveName(ctx, m, nameArgs{})); !strings.Contains(got, "cannot be relied on") {
		t.Errorf("what is no record: %s", got)
	}
}

func TestAFileIsPinnedForATimeOrUntilReleased(t *testing.T) {
	ctx := context.Background()
	k := &kept{}
	m := reaching(t, k)
	// The answer lists every pin on the file, since it is kept until the
	// last of them has lapsed, and none on any other file.
	week := timestamppb.New(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	k.pins = []*pb.Pin{{Cid: fileA, Owner: "job:job-1", ExpiresAt: week}, {Cid: fileA, Owner: "user"}, {Cid: fileB, Owner: "recent", ExpiresAt: week}}
	forGood := told(pinFile(ctx, m, pinArgs{CID: fileA}))
	if has(t, "a pin until released", forGood, `"pinned":"`+fileA+`"`, `"held_for":"user","until":"it is released"`, `"held_for":"job:job-1","until":"2026-10-16T12:00:00Z"`); strings.Contains(forGood, "recent") || k.pinned.GetCid() != fileA || k.pinned.GetTtlSeconds() != 0 {
		t.Errorf("a pin until released: %s, the node asked for %v", forGood, k.pinned)
	}
	if day := told(pinFile(ctx, m, pinArgs{CID: fileA, TTLHours: 24})); k.pinned.GetTtlSeconds() != 86400 {
		t.Errorf("a pin for a day: %s, the node asked for %v", day, k.pinned)
	}
	// A file pinned whose pins cannot then be listed is pinned all the same.
	k.failing = "ListPins"
	if got := told(pinFile(ctx, m, pinArgs{CID: fileA})); !strings.Contains(got, `"pinned":"`+fileA+`"`) || !strings.Contains(got, "could not then be listed: the node is down") {
		t.Errorf("a pin whose fellows cannot be listed: %s", got)
	}
	k.failing = ""
	if got := told(pinFile(ctx, m, pinArgs{CID: fileA, TTLHours: 0.5})); k.pinned.GetTtlSeconds() != 1800 {
		t.Errorf("a pin for half an hour: %s, the node asked for %v", got, k.pinned)
	}
	has(t, "a pin released", told(unpinFile(ctx, m, cidArgs{CID: fileA})), `{"unpinned":"`+fileA+`"}`)
	has(t, "garbage collected", told(collectGarbage(ctx, m, none{})), `"expired_pins_dropped":2`, `"blocks_removed":7`, `"bytes_freed":4096`)
}

func TestWhatFollowersHoldIsFetchedBackAndWhatCouldNotBeIsNamed(t *testing.T) {
	ctx := context.Background()
	k := &kept{restored: &pb.RestoreResponse{}}
	m := reaching(t, k)
	has(t, "nothing to restore", told(restoreFiles(ctx, m, none{})), `"restored":0`, "followers hold nothing from an earlier store")
	k.restored = &pb.RestoreResponse{Restored: 4}
	if got := told(restoreFiles(ctx, m, none{})); !strings.Contains(got, `"restored":4`) || strings.Contains(got, "note") || strings.Contains(got, "failed") {
		t.Errorf("all restored: %s", got)
	}
	k.restored = &pb.RestoreResponse{Restored: 1, Failed: []string{fileA}}
	if got := told(restoreFiles(ctx, m, none{})); !strings.Contains(got, `"could_not_be_fetched_from_any_follower":["`+fileA+`"]`) || !strings.Contains(got, `"failed":1`) || strings.Contains(got, "failed_not_named") {
		t.Errorf("one not restored: %s", got)
	}
	for range mostNamed + 1 {
		k.restored.Failed = append(k.restored.Failed, fileB)
	}
	has(t, "many not restored", told(restoreFiles(ctx, m, none{})), fmt.Sprintf(`"failed":%d`, mostNamed+2), `"failed_not_named":2`)
}

func TestOnlyTheToolsThatNeedTheMainAddressFailForWantOfIt(t *testing.T) {
	ctx := context.Background()
	call := func(session *mcp.ClientSession, tool string) (string, bool) {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return result.Content[0].(*mcp.TextContent).Text, result.IsError
	}
	n := &node{workers: []*nodepb.Worker{{Name: "rig", TaskSlots: 4}}}

	// Told of no main address, or of one it cannot reach, the server says
	// so of the tools that need it and goes on answering the others.
	untold, _ := agentOf(t, n, Config{})
	if got, failed := call(untold, "list_pins"); !failed || !strings.Contains(got, "was not told where the node's main address is") {
		t.Errorf("list_pins with no main address: %s", got)
	}
	reached := 0
	k := &kept{pins: []*pb.Pin{{Cid: fileA, Owner: "user"}}}
	m := reaching(t, k)
	session, _ := agentOf(t, n, Config{Main: func() (*Main, error) {
		if reached++; reached == 1 {
			return nil, errors.New("read the node's key: permission denied")
		}
		return m, nil
	}})
	if got, failed := call(session, "list_pins"); !failed || !strings.Contains(got, "cannot be reached as its owner") || !strings.Contains(got, "read the node's key: permission denied") {
		t.Errorf("list_pins with a main address that cannot be reached: %s", got)
	}
	if got, failed := call(session, "pool_status"); failed || !strings.Contains(got, `"name":"rig"`) {
		t.Errorf("pool_status meanwhile: %s", got)
	}
	// Asked again, it is reached. The local API's token is not shown there.
	if got, failed := call(session, "list_pins"); failed || !strings.Contains(got, `"held_for":"user"`) || len(k.shownWith) != 0 {
		t.Errorf("list_pins once the main address is reached: %s, shown %v", got, k.shownWith)
	}

	// The prompt for keeping a result says for how long, if told.
	for asked, want := range map[string]string{"": "until it is unpinned", "30 days": "for 30 days"} {
		prompt, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "keep-result", Arguments: map[string]string{"job_id": "job-9", "for": asked}})
		if err != nil {
			t.Fatal(err)
		}
		has(t, "keep-result", prompt.Messages[0].Content.(*mcp.TextContent).Text, "job job-9", want, "pin_file", "storage_status", "get_job_record")
	}
}

func TestAJobIsSharedOutAsTheAgentSays(t *testing.T) {
	for said, want := range map[string]nodepb.JobMode{
		"": nodepb.JobMode_JOB_MODE_UNSPECIFIED, "distributed": nodepb.JobMode_JOB_MODE_DISTRIBUTED, "full-worker": nodepb.JobMode_JOB_MODE_FULL_WORKER,
	} {
		if got, err := jobMode(said); err != nil || got != want {
			t.Errorf("mode %q = %v, %v", said, got, err)
		}
	}
	if _, err := serving(&node{}).run(context.Background(), runJobArgs{Workload: "primes", Mode: "sideways"}); err == nil || !strings.Contains(err.Error(), `not "sideways"`) {
		t.Errorf("a mode there is not: %v", err)
	}
}
