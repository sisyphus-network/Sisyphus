package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

func TestPlannerCanForecastAnAttachedPrivateSeries(t *testing.T) {
	const cid = "bafybeigdyrzt5sfp7udm7hu76uhy6v4nl2i3u6jz7f3k2q4p5m6n7o8p9q"
	question := "Forecast this series 30 days ahead.\n\n[Sisyphus attachments]\n- \"sales.csv\" | cid:" + cid + " | image:false | private:true"
	model := &scripted{replies: []ai.Message{
		calling("forecast", "run_job", fmt.Sprintf(`{"workload":"forecast","params":{"input":%q,"horizon":30}}`, cid)),
		{Content: "The forecast is available from the completed job."},
	}}
	pool := &office{result: `{"forecast":[{"step":1,"value":12.5}],"best":{"model":"theta"}}`}
	var key sealed.Key
	key[0] = 42
	p := &Planner{
		Model: model, ModelName: "test-model", Pool: pool,
		Workloads:  []Workload{{Name: "forecast", Description: "Forecasts a time series from a stored CSV file."}},
		SealingKey: func() (sealed.Key, error) { return key, nil },
	}

	said, err := p.Run(context.Background(), []ai.Message{{Role: ai.User, Content: question}}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(said) != 3 || said[len(said)-1].Content != "The forecast is available from the completed job." {
		t.Fatalf("planner answer/history: %+v", said)
	}
	if said[1].Role != ai.ToolRole || !strings.Contains(said[1].Content, `"forecast":[{"step":1,"value":12.5}]`) {
		t.Fatalf("planner did not receive the forecast result: %+v", said[1])
	}
	var sawResult bool
	for _, message := range model.asked[1].Messages {
		if message.Role == ai.ToolRole && strings.Contains(message.Content, `"forecast":[{"step":1,"value":12.5}]`) {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatal("forecast result was not returned to the planner model")
	}
	if !strings.Contains(model.asked[0].Messages[0].Content, "- forecast: Forecasts a time series from a stored CSV file.") ||
		model.asked[0].Messages[1].Content != question {
		t.Fatalf("planner did not receive the advertised workload and attached-file reference: %+v", model.asked[0].Messages)
	}
	if len(pool.submitted) != 1 || pool.submitted[0].GetWorkload() != "forecast" {
		t.Fatalf("planner submitted jobs: %+v", pool.submitted)
	}
	var params struct {
		Input   string `json:"input"`
		Horizon int    `json:"horizon"`
	}
	if err := json.Unmarshal(pool.submitted[0].GetParams(), &params); err != nil {
		t.Fatal(err)
	}
	if params.Input != cid || params.Horizon != 30 {
		t.Errorf("forecast parameters = %+v", params)
	}
	if len(pool.submitted[0].GetKey()) != len(key) || pool.submitted[0].GetKey()[0] != key[0] {
		t.Error("the private attachment was not submitted as a sealed job")
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
