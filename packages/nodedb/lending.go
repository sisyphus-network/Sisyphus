package nodedb

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ModelLending is how a worker's owner lends the models of an Ollama on
// their machine to the pool, and how the pool may use the machine for them.
type ModelLending struct {
	Enabled bool
	// URL is the Ollama's address, such as http://127.0.0.1:11434.
	URL string
	// Only names the models offered, as runtime.Owner's Only does; none
	// offers them all.
	Only []string
	// GPULayers is how many of a model's layers may go on graphics cards:
	// 0 for none, -1 for as many as fit. Threads and Context are the
	// processor's threads a model may use and the tokens of a conversation
	// it holds, 0 for Ollama to decide. KeepAlive is how long a model stays
	// loaded after it answered, such as "10m"; empty is Ollama's own.
	GPULayers, Threads, Context int
	KeepAlive                   string
}

// ModelLending returns how the node lends its models. One that was never
// set lends none, and leaves every limit to Ollama.
func (db *DB) ModelLending() (ModelLending, error) {
	l := ModelLending{GPULayers: -1}
	var enabled int
	var only string
	err := db.sql.QueryRow(`SELECT enabled, ollama_url, only_models, gpu_layers, threads, context, keep_alive FROM model_lending WHERE singleton_id = 1`).
		Scan(&enabled, &l.URL, &only, &l.GPULayers, &l.Threads, &l.Context, &l.KeepAlive)
	if errors.Is(err, sql.ErrNoRows) {
		return l, nil
	}
	if err != nil {
		return ModelLending{}, fmt.Errorf("load model lending: %w", err)
	}
	l.Enabled = enabled != 0
	if only != "" {
		l.Only = strings.Split(only, ",")
	}
	return l, nil
}

// SetModelLending replaces how the node lends its models.
func (db *DB) SetModelLending(l ModelLending) error {
	return db.durably("save model lending", func(b *batch) {
		b.exec(`INSERT INTO model_lending (singleton_id, enabled, ollama_url, only_models, gpu_layers, threads, context, keep_alive) VALUES (1, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (singleton_id) DO UPDATE SET enabled = excluded.enabled, ollama_url = excluded.ollama_url, only_models = excluded.only_models,
				gpu_layers = excluded.gpu_layers, threads = excluded.threads, context = excluded.context, keep_alive = excluded.keep_alive`,
			boolInt(l.Enabled), l.URL, strings.Join(l.Only, ","), l.GPULayers, l.Threads, l.Context, l.KeepAlive)
	})
}
