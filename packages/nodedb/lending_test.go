package nodedb

import (
	"reflect"
	"strings"
	"testing"
)

func TestModelLendingIsNoneUntilSetAndThenWhatWasSet(t *testing.T) {
	db, path := newDB(t)
	got, err := db.ModelLending()
	if err != nil || !reflect.DeepEqual(got, ModelLending{GPULayers: -1}) {
		t.Fatalf("lending never set = %#v, %v; want none, with Ollama deciding", got, err)
	}
	want := ModelLending{Enabled: true, URL: "http://127.0.0.1:11434", Only: []string{"llama3.1", "gemma*"}, GPULayers: 0, Threads: 8, Context: 4096, KeepAlive: "10m"}
	if err := db.SetModelLending(want); err != nil {
		t.Fatal(err)
	}
	// What is set once is replaced, not added to.
	want.Only, want.Threads = []string{"qwen3:8b"}, 4
	if err := db.SetModelLending(want); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, path)
	if got, err := db.ModelLending(); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("lending after reopening = %#v, %v; want %#v", got, err, want)
	}
	// Offering every model is kept as offering every model.
	want.Only = nil
	if err := db.SetModelLending(want); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ModelLending(); got.Only != nil {
		t.Errorf("every model offered came back as %q", got.Only)
	}
}

func TestModelLendingReportsAMissingTable(t *testing.T) {
	db, _ := newDB(t)
	if _, err := db.sql.Exec(`DROP TABLE model_lending`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ModelLending(); err == nil || !strings.Contains(err.Error(), "load model lending") {
		t.Errorf("a missing table: %v", err)
	}
	if err := db.SetModelLending(ModelLending{}); err == nil || !strings.Contains(err.Error(), "save model lending") {
		t.Errorf("saving to a missing table: %v", err)
	}
}
