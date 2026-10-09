package nodedb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PinRequest is a request made of the node as a pinning service: to keep
// the data with a CID, under a name.
type PinRequest struct {
	ID   string
	CID  string
	Name string
	// Origins are addresses the data may be fetched from, and Meta whatever
	// else the asker wanted kept with the request.
	Origins []string
	Meta    map[string]string
	// Status is how far the node has got, in the API's words: queued,
	// pinning, pinned or failed. Info says why, for one that failed.
	Status  string
	Info    string
	Created time.Time
	// Holds are the CIDs of the requests this one replaced. They are kept
	// pinned for this one until it is settled, so that what the two have in
	// common is not collected in between.
	Holds []string
}

// PinRequests lists the requests, newest first.
func (db *DB) PinRequests() ([]PinRequest, error) {
	var requests []PinRequest
	err := db.read(func(rows *sql.Rows) error {
		var r PinRequest
		var created int64
		var origins, meta, holds string
		if err := rows.Scan(&r.ID, &r.CID, &r.Name, &origins, &meta, &r.Status, &r.Info, &created, &holds); err != nil {
			return err
		}
		r.Created = moment(created)
		err := errors.Join(json.Unmarshal([]byte(origins), &r.Origins), json.Unmarshal([]byte(meta), &r.Meta), json.Unmarshal([]byte(holds), &r.Holds))
		requests = append(requests, r)
		return err
	}, `SELECT request_id, cid, name, origins, meta, status, info, created_at_ns, holds FROM pin_requests ORDER BY created_at_ns DESC, request_id`)
	if err != nil {
		return nil, fmt.Errorf("load pin requests: %w", err)
	}
	return requests, nil
}

// SavePinRequest records a request, in place of what was recorded for it
// before.
func (db *DB) SavePinRequest(r PinRequest) error {
	// Lists and maps of strings always encode.
	origins, _ := json.Marshal(r.Origins)
	meta, _ := json.Marshal(r.Meta)
	holds, _ := json.Marshal(r.Holds)
	return db.durably("save pin request", func(b *batch) {
		b.exec(`INSERT OR REPLACE INTO pin_requests (request_id, cid, name, origins, meta, status, info, created_at_ns, holds) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.CID, r.Name, string(origins), string(meta), r.Status, r.Info, nanos(r.Created), string(holds))
	})
}

// RemovePinRequest forgets a request. Forgetting one that is not there is
// not an error.
func (db *DB) RemovePinRequest(id string) error {
	return db.durably("remove pin request", func(b *batch) {
		b.exec(`DELETE FROM pin_requests WHERE request_id = ?`, id)
	})
}
