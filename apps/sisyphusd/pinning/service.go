package pinning

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

// Store is the part of a storage.Store a service keeps data in.
type Store interface {
	Has(ctx context.Context, c cid.Cid) (bool, error)
	// Verify reads the whole of a blob. Over a Kubo daemon on a network
	// that fetches what the daemon does not hold.
	Verify(ctx context.Context, c cid.Cid) error
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	Unpin(owner string, cids ...cid.Cid) error
	Size(ctx context.Context) (uint64, error)
}

// Network is the node's Kubo daemon, by which a service fetches what the
// node does not hold. A kubo.Client is one.
type Network interface {
	Connect(ctx context.Context, address string) error
	Addresses(ctx context.Context) ([]string, error)
}

// Records is where a service keeps its requests. A nodedb.DB is one.
type Records interface {
	PinRequests() ([]nodedb.PinRequest, error)
	SavePinRequest(nodedb.PinRequest) error
	RemovePinRequest(id string) error
}

// Config says what a service keeps data in and who may ask it to.
type Config struct {
	Store   Store
	Records Records
	// Network is left nil on a node without Kubo, which can then keep only
	// what it already holds.
	Network Network
	// Token is the key a request must show, as a bearer token.
	Token string
	// MaxStoreBytes is the most disk the store may use before the service
	// declines to fetch more. Zero means no limit.
	MaxStoreBytes uint64
	Log           *slog.Logger
}

// Service is a node as a pinning service. It is an http.Handler serving
// the IPFS Pinning Service API: POST /pins asks for data to be kept, GET
// /pins lists what has been asked, and GET, POST and DELETE of
// /pins/<request ID> show, replace and remove one request.
//
// Data the node holds is pinned as it is asked for. Data it does not hold
// is fetched by the node's Kubo, which first connects to the origins the
// request names, and the request goes from queued through pinning to pinned
// or failed. A node without Kubo has nothing to fetch with, so such a
// request fails at once and says so.
type Service struct {
	cfg Config
	// ctx ends when the service is closed, and the fetches with it.
	ctx  context.Context
	stop context.CancelFunc
	jobs sync.WaitGroup
	// slots holds one entry for each fetch in progress.
	slots chan struct{}

	mu       sync.Mutex
	requests map[string]*request
	// last is when the newest request was made. No two are made at the
	// same moment, since a listing is paged by it.
	last time.Time
}

// request is one request and the fetch on its behalf, if there is one. Its
// fields are guarded by the service's mutex.
type request struct {
	nodedb.PinRequest
	// cancel stops the fetch, and done is closed when it has stopped.
	cancel context.CancelFunc
	done   chan struct{}
}

// ownerPrefix begins the name a request's pin is held under in the store:
// each request has its own, so that two requests for the same data keep it
// until both are removed.
const ownerPrefix = "pinning-service:"

func owner(id string) string { return ownerPrefix + id }

var (
	// fetchesAtOnce is how many requests are fetched at a time. The rest
	// wait their turn, queued.
	fetchesAtOnce = 4
	// connectTimeout is how long Kubo is given to connect to one origin.
	connectTimeout = 10 * time.Second
	// promptness is how long a request for data the node's Kubo holds
	// waits for that data to be read through, so that the answer can be
	// "pinned" instead of leaving the asker to ask again.
	promptness = 5 * time.Second
)

// now is the time; tests set it.
var now = time.Now

// What a request is told when the node cannot fetch.
const (
	noNetwork = "this node does not hold the data and has no way to fetch it: it keeps its data on its own disk or in a bucket, not in Kubo. Store the data on the node first, or start the node with --kubo"
	notFound  = "the data could not be fetched: %w. This node's Kubo is on its pool's private IPFS network and fetches only from peers that hold the pool's swarm key; a peer on the public IPFS network cannot supply it"
)

// NewService returns a service over what cfg names, and takes up the
// fetches an earlier run left unfinished. Close it when done.
func NewService(cfg Config) (*Service, error) {
	recorded, err := cfg.Records.PinRequests()
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, ctx: ctx, stop: stop, slots: make(chan struct{}, fetchesAtOnce), requests: make(map[string]*request)}
	for _, record := range recorded {
		r := &request{PinRequest: record}
		s.requests[r.ID] = r
		if r.Created.After(s.last) {
			s.last = r.Created
		}
		if unfinished(r.Status) {
			s.start(r)
		}
	}
	return s, nil
}

// Close stops the fetches in progress, which a later run takes up again.
func (s *Service) Close() {
	s.stop()
	s.jobs.Wait()
}

func unfinished(status string) bool { return status == Queued || status == Pinning }

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	shown, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(shown), []byte(s.cfg.Token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "show this service's key as a bearer token: it is in the file pinning.token in the node's data directory")
		return
	}
	id, one := strings.CutPrefix(r.URL.Path, "/pins/")
	all := r.URL.Path == "/pins"
	one = one && id != "" && !strings.Contains(id, "/")
	var asked *request
	if one {
		if asked = s.find(id); asked == nil {
			fail(w, http.StatusNotFound, "NOT_FOUND", "there is no request "+id)
			return
		}
	}
	switch {
	case all && r.Method == http.MethodGet:
		s.list(w, r)
	case all && r.Method == http.MethodPost:
		s.add(w, r, nil)
	case one && r.Method == http.MethodGet:
		respond(w, http.StatusOK, s.status(asked, s.delegates(r.Context())))
	case one && r.Method == http.MethodPost:
		s.add(w, r, asked)
	case one && r.Method == http.MethodDelete:
		s.remove(w, asked)
	case all:
		w.Header().Set("Allow", "GET, POST")
		fail(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "requests are listed with GET and made with POST")
	case one:
		w.Header().Set("Allow", "GET, POST, DELETE")
		fail(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "a request is shown with GET, replaced with POST and removed with DELETE")
	default:
		fail(w, http.StatusNotFound, "NOT_FOUND", "this service answers at /pins and /pins/<request ID>")
	}
}

// problem is an error as the API reports it.
type problem struct {
	code            int
	reason, details string
}

func refused(details string) *problem {
	return &problem{http.StatusBadRequest, "BAD_REQUEST", details}
}

func broken(what string, err error) *problem {
	return &problem{http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", what + ": " + err.Error()}
}

func fail(w http.ResponseWriter, code int, reason, details string) {
	var f failure
	f.Error.Reason, f.Error.Details = reason, details
	respond(w, code, f)
}

func respond(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

// add takes in a request, in place of replaced if there is one.
func (s *Service) add(w http.ResponseWriter, r *http.Request, replaced *request) {
	var pin Pin
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&pin); err != nil {
		fail(w, http.StatusBadRequest, "BAD_REQUEST", "the request is not a pin object: "+err.Error())
		return
	}
	c, err := cid.Decode(pin.CID)
	var wrong *problem
	switch {
	case err != nil:
		wrong = refused(fmt.Sprintf("%q is not a CID: %v", pin.CID, err))
	case c.Type() != cid.Raw && c.Type() != cid.DagProtobuf:
		wrong = refused("this service keeps files and directories as IPFS stores them, which are raw and dag-pb blocks; " + pin.CID + " is something else")
	case len(pin.Name) > maxName:
		wrong = refused(fmt.Sprintf("a name is at most %d bytes", maxName))
	case len(pin.Origins) > maxOrigins:
		wrong = refused(fmt.Sprintf("a request names at most %d origins", maxOrigins))
	}
	if wrong != nil {
		fail(w, wrong.code, wrong.reason, wrong.details)
		return
	}

	// What the replaced request keeps is kept for the new one until that is
	// settled, so that what the two share is never without a pin.
	var holds []string
	if replaced != nil {
		s.halt(replaced)
		s.mu.Lock()
		holds = slices.Clone(replaced.Holds)
		if replaced.Status == Pinned {
			holds = append(holds, replaced.CID)
		}
		s.mu.Unlock()
	}
	taken, wrong := s.take(r.Context(), c, pin, holds)
	if wrong != nil {
		if replaced != nil && unfinished(s.status(replaced, nil).Status) {
			s.start(replaced)
		}
		fail(w, wrong.code, wrong.reason, wrong.details)
		return
	}
	if replaced != nil {
		if err := s.forget(replaced); err != nil {
			s.cfg.Log.Warn("a replaced pin request could not be removed", "request", replaced.ID, "error", err)
		}
	}
	respond(w, http.StatusAccepted, s.status(taken, s.delegates(r.Context())))
}

// take records a new request for c and does what can be done for it at
// once: pins what the node holds, starts fetching what it does not, or says
// that it cannot. holds are CIDs to keep until the request is settled.
func (s *Service) take(ctx context.Context, c cid.Cid, pin Pin, holds []string) (*request, *problem) {
	held, err := s.cfg.Store.Has(ctx, c)
	if err != nil {
		return nil, broken("look for the data", err)
	}
	if !held && s.cfg.Network != nil && s.cfg.MaxStoreBytes > 0 {
		used, err := s.cfg.Store.Size(ctx)
		if err != nil {
			return nil, broken("measure the store", err)
		}
		if used >= s.cfg.MaxStoreBytes {
			return nil, &problem{http.StatusInsufficientStorage, "INSUFFICIENT_STORAGE", fmt.Sprintf("the node's store is full: %d of %d bytes used", used, s.cfg.MaxStoreBytes)}
		}
	}

	var secret [16]byte
	rand.Read(secret[:]) // never fails; see crypto/rand
	r := &request{PinRequest: nodedb.PinRequest{
		ID: hex.EncodeToString(secret[:]), CID: c.String(), Name: pin.Name, Origins: pin.Origins, Meta: pin.Meta, Status: Queued, Holds: holds,
	}}
	keep := decoded(holds)
	switch {
	case s.cfg.Network != nil:
		// Kubo may hold the first block of something and not the rest, so
		// even what seems held is read through before it is called pinned.
	case held:
		r.Status, r.Holds = Pinned, nil
		keep = []cid.Cid{c}
	default:
		r.Status, r.Info, r.Holds = Failed, noNetwork, nil
		keep = nil
	}
	if err := s.cfg.Store.Pin(ctx, owner(r.ID), time.Time{}, keep...); err != nil {
		return nil, broken("pin the data", err)
	}

	s.mu.Lock()
	r.Created = now()
	if !r.Created.After(s.last) {
		r.Created = s.last.Add(time.Nanosecond)
	}
	s.last = r.Created
	s.requests[r.ID] = r
	record := r.PinRequest
	s.mu.Unlock()
	if err := s.cfg.Records.SavePinRequest(record); err != nil {
		s.mu.Lock()
		delete(s.requests, r.ID)
		s.mu.Unlock()
		s.cfg.Store.Unpin(owner(r.ID), keep...)
		return nil, broken("record the request", err)
	}

	if r.Status == Queued {
		done := s.start(r)
		if held {
			waited := time.NewTimer(promptness)
			defer waited.Stop()
			select {
			case <-done:
			case <-waited.C:
			case <-ctx.Done():
			}
		}
	}
	return r, nil
}

// start begins the fetch for a request, and returns what is closed when it
// has ended.
func (s *Service) start(r *request) <-chan struct{} {
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	s.mu.Lock()
	r.cancel, r.done = cancel, done
	s.mu.Unlock()
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer close(done)
		defer cancel()
		s.fetch(ctx, r)
	}()
	return done
}

// halt stops the fetch for a request, if there is one, and waits for it.
func (s *Service) halt(r *request) {
	s.mu.Lock()
	cancel, done := r.cancel, r.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// fetch brings in what a request asks for, when its turn comes, and
// settles the request as pinned or failed. A fetch that is stopped leaves the request as it stands, for whoever
// stopped it to deal with: remove it, or take it up again.
func (s *Service) fetch(ctx context.Context, r *request) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return
	}
	s.mu.Lock()
	r.Status = Pinning
	record := r.PinRequest
	s.mu.Unlock()
	s.save(record)

	c := decoded([]string{record.CID})
	err := s.bring(ctx, record, c[0])
	if ctx.Err() != nil {
		return
	}

	// Settled either way, the request lets go of what it kept for the one
	// it replaced, and if it failed, of what it had pinned of its own.
	release := slices.DeleteFunc(decoded(record.Holds), func(held cid.Cid) bool { return err == nil && held.Equals(c[0]) })
	s.mu.Lock()
	r.Status, r.Holds = Pinned, nil
	if err != nil {
		r.Status, r.Info = Failed, err.Error()
		release = append(release, c...)
	}
	record = r.PinRequest
	s.mu.Unlock()
	if err := s.cfg.Store.Unpin(owner(record.ID), release...); err != nil {
		s.cfg.Log.Warn("a pin request's pins could not be released", "request", record.ID, "error", err)
	}
	s.save(record)
}

// bring has the node's Kubo connect to a request's origins and fetch what
// it asks for, and pins that. The error it returns is worded for whoever
// made the request.
func (s *Service) bring(ctx context.Context, record nodedb.PinRequest, c cid.Cid) error {
	if s.cfg.Network == nil {
		// Taken in by a run that had Kubo, and taken up by one without.
		return errors.New(noNetwork)
	}
	for _, origin := range record.Origins {
		dialling, cancel := context.WithTimeout(ctx, connectTimeout)
		err := s.cfg.Network.Connect(dialling, origin)
		cancel()
		if err != nil {
			// Not fatal: the data may be had from somebody else.
			s.cfg.Log.Debug("the node's Kubo could not connect to a pin request's origin", "request", record.ID, "origin", origin, "error", err)
		}
	}
	// Read through once to fetch, and once more when pinned: a garbage
	// collection between the two may have taken what was not yet pinned.
	err := s.cfg.Store.Verify(ctx, c)
	if err == nil {
		err = s.cfg.Store.Pin(ctx, owner(record.ID), time.Time{}, c)
	}
	if err == nil {
		err = s.cfg.Store.Verify(ctx, c)
	}
	if err != nil {
		return fmt.Errorf(notFound, err)
	}
	return nil
}

// save records a request as it now stands. If that fails the service
// carries on with what it has in memory.
func (s *Service) save(record nodedb.PinRequest) {
	if err := s.cfg.Records.SavePinRequest(record); err != nil {
		s.cfg.Log.Warn("a pin request could not be recorded", "request", record.ID, "error", err)
	}
}

// remove stops a request's fetch and forgets the request, so that garbage
// collection can take the data.
func (s *Service) remove(w http.ResponseWriter, r *request) {
	s.halt(r)
	if err := s.forget(r); err != nil {
		fail(w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "remove the request: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// forget releases everything pinned for a request, and removes it.
func (s *Service) forget(r *request) error {
	s.mu.Lock()
	record := r.PinRequest
	s.mu.Unlock()
	if err := s.cfg.Store.Unpin(owner(record.ID), decoded(append(record.Holds, record.CID))...); err != nil {
		return err
	}
	if err := s.cfg.Records.RemovePinRequest(record.ID); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.requests, record.ID)
	s.mu.Unlock()
	return nil
}

func (s *Service) find(id string) *request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[id]
}

// decoded parses CIDs the service wrote down itself.
func decoded(texts []string) []cid.Cid {
	cids := make([]cid.Cid, 0, len(texts))
	for _, text := range texts {
		if c, err := cid.Decode(text); err == nil {
			cids = append(cids, c)
		}
	}
	return cids
}

// delegates are the addresses of the node's Kubo, which whoever has the
// data may connect to so that it can be fetched. A node without Kubo has
// none.
func (s *Service) delegates(ctx context.Context) []string {
	if s.cfg.Network == nil {
		return []string{}
	}
	addresses, err := s.cfg.Network.Addresses(ctx)
	if err != nil {
		s.cfg.Log.Debug("the node's Kubo did not give its addresses", "error", err)
	}
	return append([]string{}, addresses...)
}

// status describes a request as the API shows it.
func (s *Service) status(r *request, delegates []string) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return statusOf(r.PinRequest, delegates)
}

func statusOf(r nodedb.PinRequest, delegates []string) Status {
	status := Status{
		RequestID: r.ID, Status: r.Status, Created: r.Created.UTC(), Delegates: delegates,
		Pin: Pin{CID: r.CID, Name: r.Name, Origins: r.Origins, Meta: r.Meta},
	}
	if r.Info != "" {
		status.Info = map[string]string{Details: r.Info}
	}
	return status
}

// list answers GET /pins: the requests that pass the filters asked for,
// newest first, and no more of them than the limit.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	wanted, err := filterFrom(r.URL.Query())
	if err != nil {
		fail(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	var found []nodedb.PinRequest
	s.mu.Lock()
	for _, made := range s.requests {
		if wanted.passes(made.PinRequest) {
			found = append(found, made.PinRequest)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(found, func(a, b nodedb.PinRequest) int { return b.Created.Compare(a.Created) })
	page := results{Count: len(found), Results: []Status{}}
	delegates := s.delegates(r.Context())
	for _, made := range found[:min(len(found), wanted.limit)] {
		page.Results = append(page.Results, statusOf(made, delegates))
	}
	respond(w, http.StatusOK, page)
}

// filter is what a listing is narrowed to.
type filter struct {
	cids          map[string]bool
	named         func(name string) bool
	statuses      []string
	before, after time.Time
	meta          map[string]string
	limit         int
}

// filterFrom reads a listing's filters from its query.
func filterFrom(query url.Values) (*filter, error) {
	// With nothing asked, a listing is of the ten newest that are pinned.
	f := &filter{statuses: []string{Pinned}, limit: 10, named: func(string) bool { return true }}
	if asked := query.Get("cid"); asked != "" {
		texts := strings.Split(asked, ",")
		if len(texts) > maxCIDs {
			return nil, fmt.Errorf("a listing is filtered by at most %d CIDs", maxCIDs)
		}
		f.cids = make(map[string]bool)
		for _, text := range texts {
			c, err := cid.Decode(text)
			if err != nil {
				return nil, fmt.Errorf("%q is not a CID: %v", text, err)
			}
			f.cids[c.String()] = true
		}
	}
	if name := query.Get("name"); name != "" {
		switch query.Get("match") {
		case "", "exact":
			f.named = func(n string) bool { return n == name }
		case "iexact":
			f.named = func(n string) bool { return strings.EqualFold(n, name) }
		case "partial":
			f.named = func(n string) bool { return strings.Contains(n, name) }
		case "ipartial":
			f.named = func(n string) bool { return strings.Contains(strings.ToLower(n), strings.ToLower(name)) }
		default:
			return nil, errors.New("match is exact, iexact, partial or ipartial")
		}
	}
	if asked := query.Get("status"); asked != "" {
		f.statuses = strings.Split(asked, ",")
		for _, status := range f.statuses {
			if !slices.Contains([]string{Queued, Pinning, Pinned, Failed}, status) {
				return nil, fmt.Errorf("%q is not a status: those are queued, pinning, pinned and failed", status)
			}
		}
	}
	for name, into := range map[string]*time.Time{"before": &f.before, "after": &f.after} {
		if asked := query.Get(name); asked != "" {
			at, err := time.Parse(time.RFC3339Nano, asked)
			if err != nil {
				return nil, fmt.Errorf("%s is a time such as 2026-10-09T12:00:00Z, not %q", name, asked)
			}
			*into = at
		}
	}
	if asked := query.Get("limit"); asked != "" {
		limit, err := strconv.Atoi(asked)
		if err != nil || limit < 1 || limit > maxLimit {
			return nil, fmt.Errorf("limit is a number from 1 to %d", maxLimit)
		}
		f.limit = limit
	}
	if asked := query.Get("meta"); asked != "" {
		if err := json.Unmarshal([]byte(asked), &f.meta); err != nil {
			return nil, errors.New(`meta is a JSON object of strings, such as {"app":"mine"}`)
		}
	}
	return f, nil
}

func (f *filter) passes(r nodedb.PinRequest) bool {
	for key, value := range f.meta {
		if got, ok := r.Meta[key]; !ok || got != value {
			return false
		}
	}
	return (f.cids == nil || f.cids[r.CID]) && f.named(r.Name) && slices.Contains(f.statuses, r.Status) &&
		(f.before.IsZero() || r.Created.Before(f.before)) && (f.after.IsZero() || r.Created.After(f.after))
}
