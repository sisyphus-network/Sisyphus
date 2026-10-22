package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// A forecast's candidates are shared out across the pool, and each task is
// run by two workers whose backtests must come to the same bytes; the job
// then forecasts what the same job run in one process does.
func TestAForecastIsBacktestedOnTwoWorkersThatAgree(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.startWorker("b", 2)
	p.waitForWorkers(2)

	var series strings.Builder
	series.WriteString("week,orders\n")
	for i := range 80 {
		fmt.Fprintf(&series, "%d,%.2f\n", i, 50+0.3*float64(i)+6*math.Cos(2*math.Pi*float64(i)/7)+float64(i%3))
	}
	input, err := blobclient.Upload(p.ctx, p.blobs, strings.NewReader(series.String()))
	if err != nil {
		t.Fatal(err)
	}
	params := `{"input":"` + input.String() + `","horizon":7,"season":7,"folds":3}`
	job := p.wait(p.submit(&pb.JobSpec{Workload: "forecast", Params: []byte(params), MaxTasks: 3, Verify: 2}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	if len(job.GetTasks()) != 3 {
		t.Errorf("the candidates were shared among %d tasks", len(job.GetTasks()))
	}
	if record := p.recordText(job.GetJobId()); strings.Count(record, `"agreed":true`) != 6 || strings.Contains(record, `"agreed":false`) {
		t.Errorf("the workers did not all agree: %s", record)
	}

	var got runtime.ForecastResult
	if err := json.Unmarshal(job.GetResult(), &got); err != nil {
		t.Fatal(err)
	}
	local := runtime.Builtin()
	w, _ := local.Get("forecast")
	payloads, err := w.Split(p.ctx, p.store, []byte(params), 1)
	if err != nil {
		t.Fatal(err)
	}
	output, err := w.Execute(p.ctx, p.store, payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := w.Aggregate(p.ctx, p.store, [][]byte{output})
	if err != nil {
		t.Fatal(err)
	}
	var want runtime.ForecastResult
	json.Unmarshal(raw, &want)
	if got.Best != want.Best || fmt.Sprint(got.Forecast) != fmt.Sprint(want.Forecast) || got.ForecastTable != want.ForecastTable || got.Points != 80 || got.Last != "79" {
		t.Errorf("the pool forecast %+v with %v, where one process forecasts %+v with %v", got.Best, got.Forecast, want.Best, want.Forecast)
	}
}
