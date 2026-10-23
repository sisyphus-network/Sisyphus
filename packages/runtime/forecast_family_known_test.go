package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// What forecast and forecasts do with Chronos-Bolt is known to the last
// byte, through an image that answers as the naive model does: 40 series
// of every length from too short to backtest up, with and without
// Chronos-Bolt, with its image answering and failing, on nodes that can
// run it and nodes that cannot. A change to how the families are wired in
// that changes any task's output, what the image is given, a job's result
// or an error is caught here; a change meant to make one is made with a
// new answer.
func TestChronosWiringGivesTheKnownAnswer(t *testing.T) {
	digest := sha256.New()
	put := func(k string, v any) { fmt.Fprintf(digest, "%s\t%v\n", k, v) }
	store := storage.NewMemory()
	panel := map[string]string{}
	for i := range 40 {
		panel[fmt.Sprintf("s%02d", i)] = seasonalSeries(3 + 2*i)
	}
	input, _ := store.Put(ctx, strings.NewReader(panelOf(panel)))
	one, _ := store.Put(ctx, strings.NewReader(seasonalSeries(60)))
	for _, failing := range []bool{false, true} {
		var asked chronosAsked
		answer := func(answers []map[string]any) any { return answers }
		if failing {
			// The image leaves nothing, as when it runs out of memory.
			answer = func([]map[string]any) any { return nil }
		}
		engine := pretendChronos(t, &asked, answer)
		f := Forecast{Containers: true, Engine: engine.engine}
		fs := Forecasts{Containers: true, Engine: engine.engine}
		for _, models := range []string{"naive,chronos-tiny", "naive,mean,drift,snaive,ses,holt,holtwinters,ar,theta,chronos-base,chronos-mini", "chronos-small", "naive,ses,holtwinters", "all,chronos-base"} {
			key := fmt.Sprint(failing, " ", models)
			params := mustJSON(ForecastsParams{Input: input.String(), Horizon: 6, Season: 12, Models: models})
			payloads, err := fs.Split(ctx, store, params, 3)
			put("forecasts split "+key, fmt.Sprint(len(payloads), err))
			var outputs [][]byte
			for i, p := range payloads {
				out, err := fs.Execute(ctx, store, p)
				put(fmt.Sprintf("forecasts task %s %d", key, i), fmt.Sprint(string(out), err))
				put(fmt.Sprintf("forecasts image asked %s %d", key, i), string(mustJSON(asked)))
				outputs = append(outputs, out)
			}
			result, err := fs.Aggregate(ctx, store, outputs)
			put("forecasts result "+key, fmt.Sprint(string(result), err))
			put("forecasts needs "+key, fs.Needs(params))
			single := []byte(`{"input":"` + one.String() + `","horizon":6,"season":12,"models":"` + models + `"}`)
			payloads, err = f.Split(ctx, store, single, 3)
			put("forecast split "+key, fmt.Sprint(len(payloads), err))
			outputs = nil
			for i, p := range payloads {
				out, err := f.Execute(ctx, store, p)
				put(fmt.Sprintf("forecast task %s %d", key, i), fmt.Sprint(string(out), err))
				outputs = append(outputs, out)
			}
			result, err = f.Aggregate(ctx, store, outputs)
			put("forecast result "+key, fmt.Sprint(string(result), err))
			put("forecast needs "+key, f.Needs(single))
		}
	}
	for _, node := range []Forecast{{}, {Containers: true}, {Containers: true, Confined: Confinement{MemoryMB: 512}}} {
		put("offers", node.Offers(ctx))
		put("choices", node.modelChoices())
		put("help", node.modelsHelp())
		_, err := node.Split(ctx, store, []byte(`{"input":"`+one.String()+`","horizon":6,"models":"naive,chronos-tiny"}`), 1)
		put("forecast refused", err)
		_, err = Forecasts{Containers: node.Containers, Confined: node.Confined}.Split(ctx, store, mustJSON(ForecastsParams{Input: input.String(), Horizon: 6, Models: "chronos-tiny"}), 1)
		put("forecasts refused", err)
		if !node.Containers || node.Confined.MemoryMB > 0 {
			_, err = node.Execute(ctx, store, mustJSON(forecastTask{Input: one.String(), Horizon: 4, Folds: 2, Models: []forecastModel{{Kind: "chronos", Variant: "tiny"}}}))
			put("task refused", err)
		}
	}
	_, err := (ForecastParams{Models: "chronos-huge"}).kinds()
	put("unknown kind", err)
	for _, m := range []forecastModel{{Kind: "chronos", Variant: "base"}, {Kind: "chronos", Variant: "huge"}, {Kind: "chronos"}, {Kind: "chronos", Variant: "tiny", Season: 12}, {Kind: "naive", Variant: "tiny"}, {Kind: "prophet"}} {
		put("model "+m.Name(), fmt.Sprint(m.invalid(), " ", m.fewest()))
	}
	put("image", ChronosImage)
	put("label", ChronosLabel)
	const want = "a9a8b02e509a604c85452286458db9e3e354ae7911efcf166772f8e5fa723ae6"
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Errorf("the Chronos wiring hashes to %s, want %s", got, want)
	}
}
