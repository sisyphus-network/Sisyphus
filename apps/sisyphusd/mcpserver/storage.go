package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/names"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// Main is the node as its owner reaches it at its main address, which is
// where the command line reaches it: what it keeps and for how long, the
// copies other nodes hold, its name, and the records of its jobs. The local
// API has none of these.
type Main struct {
	Blobs pb.BlobServiceClient
	Names pb.NameServiceClient
	Jobs  pb.NodeServiceClient
	Pool  pb.PoolServiceClient
	// Identity is the node's own, which signs what it publishes under its
	// name.
	Identity *identity.Identity
	// SealingKey returns the key the node seals private jobs with, or says
	// why there is none to return.
	SealingKey func() ([]byte, error)
}

// main returns the node's main services, or why they cannot be had. Only
// the tools that need them fail for the want of them.
func (s *server) main() (*Main, error) {
	if s.Main == nil {
		return nil, errors.New("this server was not told where the node's main address is, so it cannot reach what the node keeps, its names or its jobs' records")
	}
	main, err := s.Main()
	if err != nil {
		return nil, fmt.Errorf("the node's main address cannot be reached as its owner, which is how what it keeps, its names and its jobs' records are reached: %w", err)
	}
	return main, nil
}

// keeping makes a tool of something done at the node's main address. The
// local API's token is not shown there: the node's key is what says who is
// calling.
func keeping[In any](s *server, do func(context.Context, *Main, In) (any, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		main, err := s.main()
		if err != nil {
			return shown(nil, err)
		}
		return shown(do(ctx, main, in))
	}
}

// storage gives a server the tools for what the node keeps: pins, copies
// on other nodes, names, and the records of jobs.
func (s *server) storage(out *mcp.Server) {
	seen := &mcp.ToolAnnotations{ReadOnlyHint: true}
	gone := &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
	add(s, out, reads, &mcp.Tool{Name: "list_pins", Annotations: seen,
		Description: "Lists what the node keeps and until when. A stored file is kept until the last pin on it has lapsed. held_for says whose each pin is: user (store_file and pin_file place it), job:<id> (a job's inputs and outputs, for seven days after it ends unless the node's owner set another time), record:<id> (a finished job's record), recent (the hour every new file gets), restored (taken back from another node after the store was lost). Give cid to see the pins on one file."}, keeping(s, listPins))
	add(s, out, reads, &mcp.Tool{Name: "storage_status", Annotations: seen,
		Description: "Says whether other nodes hold copies of what this node has pinned. kept_by says how the pool keeps copies. If followers (storage followers each hold a copy) or cluster (the pool's IPFS Cluster does, and its counts include this node), the answer lists each pinned file with how many copies it has, who holds them and how many were wanted. If this node alone, no other node is asked to hold anything and there is nothing more to list. Give cid to ask about one file."}, keeping(s, storageStatus))
	add(s, out, reads, &mcp.Tool{Name: "get_job_record", Annotations: seen,
		Description: "Returns the record of a job that is over: its history as linked data, named by one content ID, record_cid, that changes if anything in the history does. nodes holds the record's parts by their own IDs: what was asked, which worker ran each attempt at each task, and the IDs of its inputs and outputs. In them a link to another part or file is {\"/\": \"<content ID>\"} and bytes, such as the parameters and the result, are {\"/\": {\"bytes\": \"<base64 without padding>\"}}. With verify, the node works the record out again from the job as it holds it now: verified is true if that gives the same ID, and the tool fails if it does not. That shows the job has not changed since its record was written; it is the node checked against itself, not by anyone else."}, keeping(s, jobRecord))
	add(s, out, reads, &mcp.Tool{Name: "resolve_name", Annotations: seen,
		Description: "Says which file a name stands for now. A name is a node's ID and stands for whichever file that node last published under it. This node is asked, which answers for its own name and for the nodes in its pool. The answer is a record the name's node signed, which is checked here: one that is not that node's, or has run out, is refused. sequence numbers the name's records from 0, each publishing one more than the last."}, keeping(s, resolveName))
	add(s, out, uses, &mcp.Tool{Name: "pin_file",
		Description: "Keeps a file the node holds, by content ID, until unpin_file or for ttl_hours. Use it on a job's stored outputs, which the job's own pin keeps for seven days after it ends unless the node's owner set another time. The pin is the user's, beside any others, and a file goes only when the last pin on it has lapsed: so this makes a file last longer and never shorter, and the answer lists every pin that holds it, as list_pins does. A file has one pin of the user's: pinning it again replaces that pin's time."}, keeping(s, pinFile))
	add(s, out, uses, &mcp.Tool{Name: "unpin_file", Annotations: gone,
		Description: "Releases the user's pin on a file. The file goes once nothing else pins it and the store next clears away what is unpinned; list_pins with its cid shows what still holds it."}, keeping(s, unpinFile))
	add(s, out, uses, &mcp.Tool{Name: "publish_name",
		Description: "Points this node's name at a file, in a record signed with the node's key. The name is not chosen: it is the node's ID, and a node has the one, so this replaces whatever the name stood for (resolve_name with no name shows what that is). It is for a result that changes: whoever has the name finds the latest file with resolve_name. The record is good for lifetime_hours and nothing publishes it again. Publishing does not keep the file: pin it too."}, keeping(s, publishName))
	add(s, out, admin, &mcp.Tool{Name: "collect_garbage", Annotations: gone,
		Description: "Has the node's store drop pins that have run out and delete everything no pin holds, now instead of when it next would. Says how much was freed."}, keeping(s, collectGarbage))
	add(s, out, admin, &mcp.Tool{Name: "restore_files",
		Description: "For a node whose store was lost: fetches back what its storage followers still hold from the store it had before, on their word, and pins it for the user. A node that still has its key takes back by itself what it had signed for; this is for the rest, which storage_status counts as unsigned."}, keeping(s, restoreFiles))

	out.AddPrompt(&mcp.Prompt{Name: "keep-result", Title: "Keep a job's result safe",
		Description: "Pin what a job made, check its copies and its record, and give it a name if it is to be found again.",
		Arguments: []*mcp.PromptArgument{
			{Name: "job_id", Description: "The job whose result to keep.", Required: true},
			{Name: "for", Description: "How long to keep it, such as 30 days. For good, if left out."},
		}},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			long := "until it is unpinned"
			if asked := req.Params.Arguments["for"]; asked != "" {
				long = "for " + asked
			}
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "Keep what job " + req.Params.Arguments["job_id"] + " on the Sisyphus pool made, " + long + ".\n\n" +
				"get_job gives its stored_outputs: pin_file each of them, with ttl_hours if it is for a time. list_pins with each cid shows the pin and until when. " +
				"storage_status says whether any other node holds a copy; say plainly if this node holds the only one. " +
				"get_job_record with verify gives the ID of the job's record, which is what to quote as proof of what ran. " +
				"Only if I ask for a name for the result: publish_name the output, then resolve_name to check it, and tell me when the record runs out. " +
				"Report the content IDs kept and until when, how many copies there are, and the record's ID."}}}}, nil
		})
}

// mostPins is how many pins list_pins lists, mostCopies how many files
// storage_status says the holders of, and mostNamed how many node IDs or
// content IDs any one list in an answer names.
const (
	mostPins   = 200
	mostCopies = 100
	mostNamed  = 50
)

// some returns the first of a list of IDs, and how many were left off.
func some(ids []string) (first []string, more int) {
	if len(ids) > mostNamed {
		return ids[:mostNamed], len(ids) - mostNamed
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, 0
}

// contentIDOf reads a content ID an agent gave, if it gave one, and returns
// it as the node writes it.
func contentIDOf(given string) (string, error) {
	if given == "" {
		return "", nil
	}
	c, err := cid.Decode(given)
	if err != nil {
		return "", fmt.Errorf("invalid CID %q: %w", given, err)
	}
	return c.String(), nil
}

// hours is so many hours as a length of time, if it is one.
func hours(n float64, what string) (time.Duration, error) {
	// A hundred years: beyond it a length of time no longer fits its type.
	const most = 100 * 366 * 24
	if n < 0 || n > most {
		return 0, fmt.Errorf("%s must be between 0 and %d", what, most)
	}
	return time.Duration(n * float64(time.Hour)), nil
}

type pinsArgs struct {
	CID string `json:"cid,omitempty" jsonschema:"list only the pins on this file; leave out for every pin"`
}

func listPins(ctx context.Context, m *Main, args pinsArgs) (any, error) {
	only, err := contentIDOf(args.CID)
	if err != nil {
		return nil, err
	}
	listed, err := m.Blobs.ListPins(ctx, &pb.ListPinsRequest{})
	if err != nil {
		return nil, err
	}
	pins, count := []map[string]any{}, 0
	for _, pin := range listed.GetPins() {
		if only != "" && pin.GetCid() != only {
			continue
		}
		if count++; len(pins) == mostPins {
			continue
		}
		until := "it is released"
		if pin.GetExpiresAt() != nil {
			until = pin.GetExpiresAt().AsTime().UTC().Format(time.RFC3339)
		}
		pins = append(pins, map[string]any{"cid": pin.GetCid(), "held_for": pin.GetOwner(), "until": until})
	}
	out := map[string]any{"pins": pins, "count": count}
	switch {
	case count > len(pins):
		out["note"] = fmt.Sprintf("only the first %d of %d pins are listed: give cid to see the pins on one file", len(pins), count)
	case count == 0 && only != "":
		out["note"] = "nothing pins this file: if the node still holds it, it goes when the store next clears away what is unpinned"
	}
	return out, nil
}

type copiesArgs struct {
	CID string `json:"cid,omitempty" jsonschema:"report on this file only; leave out for everything pinned"`
}

func storageStatus(ctx context.Context, m *Main, args copiesArgs) (any, error) {
	only, err := contentIDOf(args.CID)
	if err != nil {
		return nil, err
	}
	followers, err := m.Blobs.Replicas(ctx, &pb.ReplicasRequest{Cid: only})
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if only != "" {
		out["cid"] = only
	}
	// A node that runs no cluster says so by refusing, which is no failure.
	cluster, err := m.Pool.ClusterStatus(ctx, &pb.ClusterStatusRequest{})
	switch {
	case status.Code(err) == codes.FailedPrecondition:
	case err != nil:
		return nil, err
	default:
		out["kept_by"], out["cluster"] = "cluster", clusterView(cluster, only)
	}
	if cluster == nil && followers.GetWanted() > 0 {
		out["kept_by"], out["followers"] = "followers", followersView(followers)
	}
	if out["kept_by"] == nil {
		out["kept_by"] = "this node alone"
		out["note"] = "no other node is asked to hold copies of what this node pins: it was started without --replicas and without --cluster, so its own disk holds the only copy"
	}
	// What a node that lost its store has taken back, or has yet to.
	if followers.GetRestoring() || followers.GetRestored() > 0 || followers.GetFromEarlierStore() > 0 {
		lost := map[string]any{
			"restoring": followers.GetRestoring(), "restored": followers.GetRestored(),
			"held_by_followers_from_an_earlier_store": followers.GetFromEarlierStore(), "of_those_unsigned": followers.GetUnsignedFromEarlierStore(),
		}
		if followers.GetUnsignedFromEarlierStore() > 0 {
			lost["note"] = "the node takes back by itself what a list it signed names; the unsigned are taken back only by restore_files, or sisyphusd blob restore"
		}
		out["after_a_lost_store"] = lost
	}
	return out, nil
}

// followersView is the copies a node's storage followers hold.
func followersView(r *pb.ReplicasResponse) map[string]any {
	wanted := int(r.GetWanted())
	ids, more := some(r.GetFollowers())
	out := map[string]any{"followers_connected": len(r.GetFollowers()), "followers": ids, "copies_wanted": wanted}
	if more > 0 {
		out["followers_not_named"] = more
	}
	if short := wanted - len(r.GetFollowers()); short > 0 {
		out["too_few_followers_by"] = short
	}
	if r.GetSettling() {
		out["settling"] = "the node started a short while ago: followers that hold copies from before are given time to say so before any are handed what it already had"
	}
	files, short := []map[string]any{}, 0
	for _, blob := range r.GetBlobs() {
		if len(blob.GetHolders()) < wanted {
			short++
		}
		if len(files) < mostCopies {
			holders, _ := some(blob.GetHolders())
			files = append(files, map[string]any{"cid": blob.GetCid(), "copies": len(blob.GetHolders()), "copies_shown": len(blob.GetShown()), "held_by": holders})
		}
	}
	out["files"], out["files_pinned"], out["files_with_too_few_copies"] = files, len(r.GetBlobs()), short
	if failed := r.GetChallengesFailed(); failed > 0 {
		out["copies_not_shown"] = fmt.Sprintf("%d time(s) since the node started, a follower asked to show it held a file it said it held did not; such a copy is not counted until it is shown", failed)
	}
	if len(files) < len(r.GetBlobs()) {
		out["note"] = fmt.Sprintf("only the first %d of %d files are listed: give cid to ask about one", len(files), len(r.GetBlobs()))
	}
	return out
}

// clusterView is a pool's cluster: its members, and which hold each pin, or
// the one pin asked after.
func clusterView(state *pb.ClusterStatusResponse, only string) map[string]any {
	wanted := int(state.GetReplicas())
	members := []map[string]any{}
	for _, member := range state.GetMembers() {
		if len(members) == mostNamed {
			break
		}
		listed := map[string]any{"node_id": member.GetNodeId(), "name": member.GetName(), "answering": member.GetError() == ""}
		if member.GetError() != "" {
			listed["error"] = member.GetError()
		}
		members = append(members, listed)
	}
	files, pinned, short := []map[string]any{}, 0, 0
	for _, pin := range state.GetPins() {
		if only != "" && pin.GetCid() != only {
			continue
		}
		pinned++
		holders, waiting := []string{}, []string{}
		for _, copied := range pin.GetCopies() {
			name := copied.GetName()
			if name == "" {
				name = copied.GetNodeId()
			}
			switch {
			case copied.GetStatus() == "pinned":
				holders = append(holders, name)
			case copied.GetError() != "":
				waiting = append(waiting, fmt.Sprintf("%s (%s: %s)", name, copied.GetStatus(), copied.GetError()))
			default:
				waiting = append(waiting, fmt.Sprintf("%s (%s)", name, copied.GetStatus()))
			}
		}
		if len(holders) < wanted {
			short++
		}
		if len(files) < mostCopies {
			held, _ := some(holders)
			file := map[string]any{"cid": pin.GetCid(), "copies": len(holders), "held_by": held}
			if late, _ := some(waiting); len(late) > 0 {
				file["waiting_for"] = late
			}
			files = append(files, file)
		}
	}
	out := map[string]any{
		"members": members, "members_heard_from": len(state.GetMembers()), "copies_wanted": wanted,
		"files": files, "files_pinned": pinned, "files_with_too_few_copies": short,
	}
	if len(files) < pinned {
		out["note"] = fmt.Sprintf("only the first %d of %d files are listed: give cid to ask about one", len(files), pinned)
	}
	return out
}

type recordArgs struct {
	JobID            string `json:"job_id" jsonschema:"the ID of a job that is over: succeeded, failed or cancelled"`
	Verify           bool   `json:"verify,omitempty" jsonschema:"have the node work the record out again from the job as it holds it now, and fail unless that is this record"`
	CheckCommitments bool   `json:"check_commitments,omitempty" jsonschema:"for a job run with private, whose record holds commitments in place of what was sealed: verify, and also check each commitment against the values the node holds, with this node's sealing key, which the node is sent for that"`
}

func jobRecord(ctx context.Context, m *Main, args recordArgs) (any, error) {
	// The commitments checked are the job's. Only a record that has been
	// checked against the job is known to hold the same ones.
	request := &pb.GetJobRecordRequest{JobId: args.JobID, Verify: args.Verify || args.CheckCommitments}
	if args.CheckCommitments {
		key, err := m.SealingKey()
		if err != nil {
			return nil, fmt.Errorf("the commitments were not checked, for want of this node's sealing key: %w", err)
		}
		request.Key = key
	}
	record, err := m.Jobs.GetJobRecord(ctx, request)
	if err != nil {
		return nil, err
	}
	if request.GetVerify() && !record.GetMatches() {
		return nil, fmt.Errorf("job %s does not match its record %s: worked out again from the job as the node holds it now, the record would be %s", record.GetJobId(), record.GetRootCid(), record.GetRecomputedCid())
	}
	out := map[string]any{"job_id": record.GetJobId(), "record_cid": record.GetRootCid()}
	if request.GetVerify() {
		out["verified"] = true
	}
	// Each node as the coordinator rendered it, for as many as there is
	// room to show.
	nodes, room, left, sealed := map[string]any{}, maxResult, 0, false
	for _, n := range record.GetNodes() {
		text := n.GetJson()
		sealed = sealed || strings.Contains(text, `_commitment"`)
		if len(text) > room {
			left++
			continue
		}
		room -= len(text)
		if nodes[n.GetCid()] = any(text); json.Valid([]byte(text)) {
			nodes[n.GetCid()] = json.RawMessage(text)
		}
	}
	out["nodes"] = nodes
	if left > 0 {
		out["note"] = fmt.Sprintf("the record is too long to show whole: %d of its %d nodes are left out, and \"sisyphusd job record %s\" prints them all", left, len(record.GetNodes()), record.GetJobId())
	}
	checks := record.GetCommitments()
	switch {
	case !args.CheckCommitments && sealed:
		out["commitments"] = "not checked: this is a private job's record, and check_commitments checks what it commits to with this node's key"
	case !args.CheckCommitments:
	case len(checks) == 0:
		out["commitments"] = "none to check: only the record of a private job holds commitments"
	default:
		var missed []string
		for _, check := range checks {
			if !check.GetMatches() {
				missed = append(missed, check.GetName())
			}
		}
		if named, more := some(missed); len(named) > 0 {
			return nil, fmt.Errorf("%d of the %d commitments in the record of job %s do not match (%s, and %d more): this node's sealing key is not the job's, or the node no longer holds the values the job ended with. The record itself was verified first and is %s",
				len(missed), len(checks), record.GetJobId(), strings.Join(named, ", "), more, record.GetRootCid())
		}
		out["commitments"] = fmt.Sprintf("checked: this node's key and the values the node holds give all %d", len(checks))
	}
	return out, nil
}

type nameArgs struct {
	Name string `json:"name,omitempty" jsonschema:"the name, which is a node's ID; this node's own if left out"`
}

// nameView is a name's record as an agent is told of it.
func nameView(record *names.Record) map[string]any {
	return map[string]any{
		"name": record.Name, "cid": record.Value.String(), "sequence": record.Sequence,
		"good_until": record.Expires.UTC().Format(time.RFC3339), "ask_again_after_seconds": int(record.TTL / time.Second),
	}
}

// resolveName asks the node what a name stands for, and believes the answer
// only as far as the name's own node signed it.
func resolveName(ctx context.Context, m *Main, args nameArgs) (any, error) {
	name := args.Name
	if name == "" {
		name = m.Identity.ID()
	}
	id, err := names.ID(name)
	if err != nil {
		return nil, err
	}
	held, err := m.Names.Resolve(ctx, &pb.ResolveNameRequest{Name: id})
	if err != nil {
		return nil, err
	}
	record, err := names.Check(id, held.GetRecord(), time.Now())
	if err != nil {
		return nil, fmt.Errorf("the node gave a record for %s that cannot be relied on: %w", id, err)
	}
	return nameView(record), nil
}

type publishArgs struct {
	CID           string  `json:"cid" jsonschema:"the content ID of the file the name is to stand for"`
	LifetimeHours float64 `json:"lifetime_hours,omitempty" jsonschema:"how long the record is good for, after which the name stands for nothing until published again; 48 if left out"`
	TTLSeconds    uint32  `json:"ttl_seconds,omitempty" jsonschema:"how long whoever reads the record may go on using it before asking again; the usual time if left out"`
}

// publishName points the node's name at a file. The record is signed here,
// with the node's key, and handed to the node, which keeps it and answers
// with it from then on.
func publishName(ctx context.Context, m *Main, args publishArgs) (any, error) {
	value, err := cid.Decode(args.CID)
	if err != nil {
		return nil, fmt.Errorf("invalid CID %q: %w", args.CID, err)
	}
	lifetime, err := hours(args.LifetimeHours, "lifetime_hours")
	if err != nil {
		return nil, err
	}
	switch {
	case lifetime == 0:
		lifetime = names.DefaultLifetime
	case lifetime < time.Second:
		return nil, errors.New("lifetime_hours must come to at least a second")
	}
	ttl := names.DefaultTTL
	if args.TTLSeconds > 0 {
		ttl = time.Duration(args.TTLSeconds) * time.Second
	}
	// A record replaces the one before it by having a higher sequence
	// number, and the node that keeps the records knows the last one used.
	id := m.Identity.ID()
	var sequence uint64
	held, err := m.Names.Resolve(ctx, &pb.ResolveNameRequest{Name: id})
	switch {
	case status.Code(err) == codes.NotFound:
	case err != nil:
		return nil, err
	default:
		last, err := names.Parse(id, held.GetRecord())
		if err != nil {
			return nil, fmt.Errorf("the node gave, as this node's last record, one that is not: %w", err)
		}
		sequence = last.Sequence + 1
	}
	record := names.Make(m.Identity, value, sequence, lifetime, ttl, time.Now())
	if _, err := m.Names.Publish(ctx, &pb.PublishNameRequest{Record: record.Bytes()}); err != nil {
		return nil, err
	}
	return nameView(record), nil
}

type pinArgs struct {
	CID      string  `json:"cid" jsonschema:"the content ID of the file to keep"`
	TTLHours float64 `json:"ttl_hours,omitempty" jsonschema:"how many hours to keep it for, 24 for a day; leave out to keep it until unpin_file"`
}

func pinFile(ctx context.Context, m *Main, args pinArgs) (any, error) {
	ttl, err := hours(args.TTLHours, "ttl_hours")
	if err != nil {
		return nil, err
	}
	// The node counts in whole seconds, and none means for good.
	seconds := uint64(ttl / time.Second)
	if ttl > 0 && seconds == 0 {
		return nil, errors.New("ttl_hours must come to at least a second")
	}
	if _, err := m.Blobs.Pin(ctx, &pb.PinBlobRequest{Cid: args.CID, TtlSeconds: seconds}); err != nil {
		return nil, err
	}
	// Every pin on the file, the new one among them: the file is kept until
	// the last has lapsed, which may be later than was asked for here.
	out := map[string]any{"pinned": args.CID}
	held, err := listPins(ctx, m, pinsArgs{CID: args.CID})
	if err != nil {
		out["note"] = "the file is pinned, but the pins on it could not then be listed: " + status.Convert(err).Message()
		return out, nil
	}
	out["pins"] = held.(map[string]any)["pins"]
	return out, nil
}

func unpinFile(ctx context.Context, m *Main, args cidArgs) (any, error) {
	if _, err := m.Blobs.Unpin(ctx, &pb.UnpinBlobRequest{Cid: args.CID}); err != nil {
		return nil, err
	}
	return map[string]any{"unpinned": args.CID}, nil
}

func collectGarbage(ctx context.Context, m *Main, _ none) (any, error) {
	done, err := m.Blobs.CollectGarbage(ctx, &pb.CollectGarbageRequest{})
	if err != nil {
		return nil, err
	}
	return map[string]any{"expired_pins_dropped": done.GetExpiredPins(), "blocks_removed": done.GetBlocksRemoved(), "bytes_freed": done.GetBytesFreed()}, nil
}

func restoreFiles(ctx context.Context, m *Main, _ none) (any, error) {
	done, err := m.Blobs.Restore(ctx, &pb.RestoreRequest{})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"restored": done.GetRestored(), "pinned": "for the user, until unpinned"}
	failed, more := some(done.GetFailed())
	switch {
	case len(failed) > 0:
		out["could_not_be_fetched_from_any_follower"], out["failed"] = failed, len(done.GetFailed())
		if more > 0 {
			out["failed_not_named"] = more
		}
	case done.GetRestored() == 0:
		out["note"] = "followers hold nothing from an earlier store that this node has not pinned"
	}
	return out, nil
}
