package planner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

func TestPrivateJobUsesNodeKey(t *testing.T) {
	pool := &office{}
	var key sealed.Key
	key[0] = 42
	p := &Planner{Pool: pool, SealingKey: func() (sealed.Key, error) { return key, nil }}
	_, err := p.runJob(context.Background(), json.RawMessage(`{"workload":"primes","params":{},"private":true}`), func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.submitted) != 1 || len(pool.submitted[0].Key) != len(key) || pool.submitted[0].Key[0] != 42 {
		t.Fatal("private job did not receive node key")
	}
}

func TestPrivateJobNeverFallsBackToPublic(t *testing.T) {
	for _, keyProvider := range []func() (sealed.Key, error){nil, func() (sealed.Key, error) { return sealed.Key{}, errors.New("key unavailable") }} {
		pool := &office{}
		p := &Planner{Pool: pool, SealingKey: keyProvider}
		_, err := p.runJob(context.Background(), json.RawMessage(`{"workload":"primes","params":{},"private":true}`), func(Event) {})
		if err == nil || len(pool.submitted) != 0 {
			t.Fatal("private job was submitted without a key")
		}
	}
}

func TestPublicJobDoesNotReadSealingKey(t *testing.T) {
	pool := &office{}
	p := &Planner{Pool: pool, SealingKey: func() (sealed.Key, error) { t.Fatal("public job read key"); return sealed.Key{}, nil }}
	_, err := p.runJob(context.Background(), json.RawMessage(`{"workload":"primes","params":{}}`), func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.submitted[0].Key) != 0 {
		t.Fatal("public job has key")
	}
}

func TestPrivateToolSchemaDoesNotExposeKey(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if string(schema.Properties["private"]) == "" {
		t.Fatal("private flag missing from tool schema")
	}
	if _, exposed := schema.Properties["key"]; exposed {
		t.Fatal("encryption key exposed to model tool arguments")
	}
}
