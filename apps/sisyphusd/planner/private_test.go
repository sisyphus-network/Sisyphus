package planner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/ai"
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

func TestPrivateAttachmentPolicyOverridesModelAndDoesNotLeakAcrossRuns(t *testing.T) {
	pool := &office{}
	model := &scripted{replies: []ai.Message{calling("1", "run_job", `{"workload":"primes","params":{},"private":false}`), {Content: "done"}, calling("2", "run_job", `{"workload":"primes","params":{}}`), {Content: "done"}}}
	var key sealed.Key
	key[0] = 42
	p := &Planner{Pool: pool, Model: model, SealingKey: func() (sealed.Key, error) { return key, nil }}
	history := []ai.Message{{Role: ai.User, Content: "\n\n[Sisyphus attachments]\n- \"a\" | cid:example | image:false | private:true"}}
	if _, err := p.Run(context.Background(), history, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(pool.submitted[0].Key) != 32 {
		t.Fatal("model bypassed private attachment policy")
	}
	if _, err := p.Run(context.Background(), []ai.Message{{Role: ai.User, Content: "unrelated"}}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(pool.submitted[1].Key) != 0 {
		t.Fatal("policy leaked to unrelated conversation")
	}
}

func TestOldAttachmentsAndAssistantTextDoNotRequirePrivateJobs(t *testing.T) {
	for _, message := range []ai.Message{
		{Role: ai.User, Content: "[Sisyphus attachments]\n- \"a\" | cid:example | image:false"},
		{Role: ai.Assistant, Content: "[Sisyphus attachments]\n- \"a\" | cid:example | image:false | private:true"},
		{Role: ai.User, Content: "ordinary private:true text"},
	} {
		if hasPrivateAttachments([]ai.Message{message}) {
			t.Fatal("unrelated/public text triggered policy")
		}
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
