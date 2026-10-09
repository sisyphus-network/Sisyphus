package nodedb

import (
	"database/sql"
	"fmt"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

// SaveStanding keeps what has been seen of one worker, in place of what was
// kept for it before. Like a job's events it is not synced to disk by
// itself; it is safe once the job's next step is.
func (db *DB) SaveStanding(s jobmodel.Standing) error {
	err := db.write(func(b *batch) {
		b.exec(`INSERT OR REPLACE INTO worker_standing (node_id, agreed, outvoted, probation) VALUES (?, ?, ?, ?)`,
			s.NodeID, int64(s.Agreed), int64(s.Outvoted), s.Probation)
	})
	if err != nil {
		return fmt.Errorf("save a worker's standing: %w", err)
	}
	return nil
}

// LoadStandings returns the standing of every worker that has one, ordered
// by node ID.
func (db *DB) LoadStandings() ([]jobmodel.Standing, error) {
	var standings []jobmodel.Standing
	err := db.read(func(rows *sql.Rows) error {
		var s jobmodel.Standing
		err := rows.Scan(&s.NodeID, &s.Agreed, &s.Outvoted, &s.Probation)
		standings = append(standings, s)
		return err
	}, `SELECT node_id, agreed, outvoted, probation FROM worker_standing ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("load workers' standing: %w", err)
	}
	return standings, nil
}
