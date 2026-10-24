# How well the forecast workload forecasts

The `forecast` workload backtests a range of candidate models on a series' own past and forecasts with the median of the best candidate of each of the best-scoring kinds of model. Whether that forecasts well is a question with a standard answer: the [M4 competition](https://github.com/Mcompetitions/M4-methods)'s 100,000 series, scored the way the competition scored its 61 entries. `packages/runtime/forecastbench` does that, so the workload's accuracy is measured rather than claimed, and so every change to it can be measured the same way.

## What is measured

Each M4 series comes in two parts: its past, which the forecast is made from, and the values that followed, which it is scored against. The series are yearly, quarterly, monthly, weekly, daily and hourly, each forecast as far ahead as the competition asked (6, 8, 18, 13, 14 and 48 steps), and the quarterly, monthly and hourly ones with their cycle (4, 12 and 24 steps).

The scores are the competition's:

- **sMAPE**, the symmetric mean absolute percentage error, in percent.
- **MASE**, the mean absolute error scaled by how far each value of the series' past is from the one a cycle before it.
- **Coverage**, how many of the values that followed fell in the forecast's 80% and 95% ranges. A range that says 95% should hold about 95 values in a hundred: fewer and it is overconfident, more and it is wider than it needs to be.
- **MSIS**, the competition's mean scaled interval score of the 95% ranges: their width plus forty times how far each value fell outside, scaled as MASE is. Lower is better; it rewards ranges that are both narrow and honest.
- **OWA**, the overall weighted average: sMAPE and MASE each as a share of those of **Naive2**, the competition's benchmark, averaged. Naive2 carries the last value forward, after taking the cycle out of a series that has one and putting it back into the forecast. An OWA of 1 is as good as Naive2; lower is better.

Each series is forecast with the workload exactly as a job would be: its candidates are split into tasks, the tasks are run, and the results aggregated, in one process. A pool forecasts the same, since the answer does not depend on how the job is split. The backtest uses as many windows of the horizon, up to four, as leave the models at least a horizon's worth of points to be fitted to. A series the workload cannot forecast would be scored as Naive2 and counted under **fell back**; on M4 there are none.

Naive2 and the scores are written from the competition's own code. Measured here, Naive2 scores an sMAPE of 13.564 and a MASE of 1.912 over all the series, the competition's published figures for it, which is the check that the scoring is the competition's.

## Running it

The data is not in the repository. Download the twelve files from the [`Dataset`](https://github.com/Mcompetitions/M4-methods/tree/master/Dataset) directory of the competition's repository, `Train/Yearly-train.csv` to `Test/Hourly-test.csv`, into one directory, then:

```sh
SISYPHUS_M4=~/m4-data go test -run 'TestM4$' -v -timeout 1h ./packages/runtime/forecastbench
```

`SISYPHUS_M4_EVERY=100` scores every hundredth series instead, a thousand in all, for a quick look. All 100,000 take under a minute on a 32-core machine. Without `SISYPHUS_M4` the test is skipped, so the ordinary test runs never need the data.

`SISYPHUS_M4_MODELS` names the models to try, as a job's `models` does, and runs each frequency as one `forecasts` job of all its series, the way a pool would forecast them together. Its forecasts are the same as forecasting each series alone. Naming a Chronos-Bolt or TimesFM kind runs it in its pinned image, so it needs Docker and the image; `all` does not include those kinds, so list the others beside them. `SISYPHUS_M4_WORKERS` sets how many of the job's tasks run at once. By default that is one for each processor, or one in all when a Chronos-Bolt or TimesFM kind is named. Each such task is a container of up to 4 GB on one thread, and the benchmark does not hold them to the machine's memory as a pool's scheduler does. Set it higher only when the machine has 4 GB free for each worker. For example, every hundredth series with Chronos-Bolt-base among the candidates, eight at a time on a machine with 32 GB to spare:

```sh
SISYPHUS_M4=~/m4-data SISYPHUS_M4_EVERY=100 SISYPHUS_M4_WORKERS=8 \
SISYPHUS_M4_MODELS=naive,mean,drift,snaive,ses,holt,holtwinters,ar,theta,chronos-base \
  go test -run 'TestM4$' -v -timeout 6h ./packages/runtime/forecastbench
```

## Results

All 100,000 series, with the workload as it is now:

| Frequency | Series | sMAPE | MASE | OWA | Naive2 sMAPE | Naive2 MASE | In 80% range | In 95% range | MSIS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Yearly | 23,000 | 13.955 | 3.077 | 0.814 | 16.342 | 3.974 | 77.1% | 93.6% | 34.14 |
| Quarterly | 24,000 | 10.404 | 1.186 | 0.905 | 11.012 | 1.371 | 81.0% | 94.3% | 10.11 |
| Monthly | 48,000 | 13.408 | 0.948 | 0.911 | 14.427 | 1.063 | 86.0% | 96.4% | 9.23 |
| Weekly | 359 | 8.724 | 2.432 | 0.914 | 9.161 | 2.777 | 87.3% | 97.8% | 18.58 |
| Daily | 4,227 | 3.075 | 3.313 | 1.010 | 3.045 | 3.278 | 81.0% | 95.4% | 28.39 |
| Hourly | 414 | 13.169 | 1.009 | 0.569 | 18.383 | 2.395 | 94.0% | 99.4% | 10.50 |
| **Overall** | **100,000** | **12.358** | **1.601** | **0.874** | **13.564** | **1.912** | **82.6%** | **95.2%** | **16.02** |

For comparison, from the competition's results: the winner, Smyl's hybrid of exponential smoothing and a recurrent network, scored an OWA of 0.821 (sMAPE 11.374, MASE 1.536), and the best single method, Theta, 0.897 (sMAPE 12.309, MASE 1.696). The workload now scores better than Theta, and better than every single method in the competition, though not yet than its best combinations.

### How it got there

Each change was measured on all 100,000 series before it was kept:

| Change | Overall OWA |
| --- | ---: |
| Forecast with the candidate of least root mean square error in the backtest ([#634](https://github.com/sisyphus-network/Sisyphus/pull/634)) | 0.983 |
| Rank the candidates by mean absolute error instead, which a few large misses do not dominate | 0.973 |
| Forecast with the median of the best candidate of each of the four best kinds, only where its backtest error is less than the best candidate's | 0.961 |
| The same median, always | 0.886 |
| Add the theta method: simple exponential smoothing with a drift of half the series' slope, the cycle taken out first as Naive2 does | 0.880 |
| **Backtest on as many windows as fit, up to eight, unless the job says otherwise**, rather than four | **0.874** |

The median of a few different kinds of model forecasts better than the backtest's winner even on series where the backtest prefers the winner: picking by a short backtest overfits it, and the median does not. It takes the best candidate of each kind, rather than the best few candidates, since the best few are often the same model with nearly the same settings. Four kinds is the number measured best here (before theta, three scored 0.902, five 0.890, six 0.921; with it, three 0.898 and five 0.884), and it was chosen on these same series, so the competition's figures, made without seeing them, are the fairer comparison by a little.

The number of windows matters because the backtest is how the best of each kind is chosen, and a choice made on a few windows is partly luck. Up to 2 windows scored 0.885, 4 scored 0.880, 8 scored 0.874, 12 scored 0.874 and 16 scored 0.877: past eight, the windows reach so far back that the series has changed since.

### The ranges

A forecast's 80% and 95% ranges come from the spread of the chosen forecast's own errors in the backtest. Taken as normal, they were overconfident: the 95% ranges held 81% of what followed, and only 70% for yearly series, whose short histories leave room for one or two backtest windows, so that the spread at each step was a guess from one or two errors. Two changes fixed that, each measured:

| Ranges | In 80% | In 95% | MSIS |
| --- | ---: | ---: | ---: |
| The backtest's error at each step, times the normal distribution's quantiles | 66.7% | 81.0% | 19.45 |
| The spread never narrower further ahead, and at least the overall error grown as the square root of the steps ahead | 76.3% | 89.1% | 16.24 |
| **And Student's t quantiles, with as many degrees of freedom as there were backtest windows** | **82.6%** | **95.2%** | **16.02** |

Student's t is the standard allowance for a spread estimated from few values: with eight windows it widens the 95% range by 18% over the normal one, with one window by more than six times. Widening every range by a flat fifth instead scored a lower MSIS, 15.27, but held only 92.8% in the 95% ranges, and its fifth was chosen on these series; the t quantiles are chosen by nothing but the number of windows, and their ranges mean what they say.

Some changes were measured and not kept:

| Tried | Overall OWA |
| --- | ---: |
| Damped-trend exponential smoothing as another kind | 0.894 |
| Damped trends as settings of Holt's smoothing instead, so they compete with it for one place | 0.882 |
| Naive's forecast always in the median, as an anchor | 0.885 |
| Naive, drift, simple and Holt's smoothing each also tried with the cycle taken out first | 0.880, no change |

A damped trend is a good model, and on its own it scores well, but here it has to earn one of the four places in the median. As a kind of its own it often does, pushing out a kind, often theta or the autoregression, whose errors differ more from the others' and so do more to cancel them; the median of four similar trend-following forecasts is no better than one of them. As settings of Holt's smoothing it cannot push anything out, but it gives the backtest twenty-four Holt candidates to choose among instead of six, and with more to choose from the one that looks best on a few windows is more often the luckiest rather than the best. Both are the same lesson: what helps a combination is models that are wrong in different ways, and fewer, sounder choices. The others changed too little to be worth their cost.

### What to try next

- **Daily series**, the one frequency where the workload is not better than Naive2. They are long and close to a random walk, which is hard to beat; a model chosen from the last few months rather than years of history may do better.
- **Foundation models as candidates of their own.** Chronos-Bolt can now be asked for as a candidate (below); whether it earns a place in the median, and how many of the four places it should have, is to be measured on M4 the same way. TimesFM can now be asked for too ([below](#timesfm)).

## Chronos-Bolt

[Chronos-Bolt](https://huggingface.co/amazon/chronos-bolt-base) is a pretrained forecasting model from Amazon, published under Apache-2.0 in four sizes. On all 100,000 M4 series, forecast with the median of its base model and nothing else, it scores an overall OWA of 0.916 against this workload's 0.874: worse on yearly series, the largest group, and clearly better on weekly (0.715 against 0.914) and hourly ones (0.401 against 0.569). The plain mean of the two scores 0.865 ([#579](https://github.com/sisyphus-network/Sisyphus/issues/579#issuecomment-6357415650)). Its training data may include part of M4, so its numbers here may flatter it.

So `forecast` can try it as a candidate like any other: ask for `chronos-tiny`, `chronos-mini`, `chronos-small` or `chronos-base` among the `models`. It is backtested on the same windows and scored the same way, and earns a place in the median only by scoring among the best kinds. `all` does not include it.

- **It runs in an image, on workers that run containers.** A node started with `--containers` offers it, and only such a node is handed a task with a Chronos-Bolt candidate. The image, `sisyphus-chronos:<hash>`, is built on the worker the first time it is needed, from a Dockerfile in the daemon: a pinned Python base image, every Python package installed from a list locked by hash, and each model's weights downloaded at a pinned Hugging Face revision and checked against its SHA-256. The hash in the image's name is of that Dockerfile and the script the container runs, so workers with the same daemon build the same image, and a task asks for workers that offer that image by name. The image is about 4 GB, 1.1 GB of it the four models' weights.
- **It runs as any container does**, confined as the worker's owner set, with no network, asking for one CPU and up to 4 GB of memory, given the series and writing its forecasts back ([Running containers](../apps/sisyphusd/README.md#running-containers)). All of a task's Chronos-Bolt fits go to one container, in batches of 64 windows, on one thread. A worker whose owner holds containers to less than 4 GB (`--containers-memory-mb`) does not offer it, and refuses a task that has it. If the image fails anyway, as when it runs out of memory, its candidates fail, with what went wrong in the task's log, and the candidates written in Go are scored as ever. Every worker gives a failed candidate the same error, so two honest workers whose images both failed agree.
- **Its forecasts are checked within a tolerance.** A network's arithmetic can come out differently in its last digits. Measured on 102 M4 series, it gives the same bits for the same settings, differs by up to one part in a million between one thread and four, and by up to one part in ten thousand between batches of 64 and of one. Its settings are fixed in the task for that reason. A forecast task with a Chronos-Bolt candidate is compared by the workload (`Compare`): every other candidate's results must be the same exactly, and each Chronos-Bolt candidate's forecast, scores and errors must be within one part in a thousand of the series' size: its largest value, or the largest value forecast if that is larger. The size is read from the task's input and given by both outputs, which must agree on it exactly, so a worker cannot widen the tolerance. A residual moves as much as a backtest window's forecast does, and a window's forecast is the size of the series, though the forecast from the series' end may be near zero. How far apart honest results are on other processors, such as arm64, is not yet measured.
- **`forecasts` takes it too**, for each of many series. There the choice of each series' forecast is made by the coordinator, not the worker: a task returns, for each series, the backtest of the best candidate of each kind written in Go and of every Chronos-Bolt model, which is all the choice is made from, and `Aggregate` chooses from those of the result taken. Two honest workers' Chronos-Bolt scores can differ in their last digits, and with them which candidate looks best; choosing once, from one result, keeps that from being a difference between them. A task of a job with Chronos-Bolt holds fewer series for that reason, since each series carries a dozen or so backtests rather than one forecast.
- **It is a forecast family**, one of two with TimesFM. What any pretrained model in an image of its own needs from `forecast` and `forecasts` is shared, most of it in `packages/runtime/forecast_family.go`: the kinds a job names it by, which workers offer and take it, the error when a node cannot run it, which runner fits its candidates, keeping all of its candidates for the coordinator's choice, and comparing two runs within its tolerance. Each family describes itself to that code (`forecastFamily`: its kind, its variants, the fewest points it forecasts from, its memory, its tolerance, its label and its runner) and keeps its own file for the rest. Chronos-Bolt's, `packages/runtime/chronos.go`, has its pins, its image, what the image is given and leaves, and how that is checked. A second family is a file of its own and an entry in `forecastFamilies`, as TimesFM's, `packages/runtime/timesfm.go`, is.

## TimesFM

[TimesFM](https://huggingface.co/google/timesfm-2.5-200m-pytorch) is a pretrained forecasting model from Google Research. Its 2.5 release, with 200M parameters, is published under Apache-2.0; its 3.0 release is not, and is not used. It is the second forecast family, wired in exactly as Chronos-Bolt is, so it is chosen the same way: ask for `timesfm-200m` among the `models` of `forecast` or `forecasts`. `all` does not include it, so a job that does not name it never runs it.

- **It runs in an image of its own**, `sisyphus-timesfm:<hash>`, built on a worker that runs containers the first time it is needed: the same pinned Python base image, CPU-only PyTorch and the `timesfm` package from a list locked by hash (the packages it shares with Chronos-Bolt at Chronos-Bolt's pins), and the model's weights downloaded at a pinned Hugging Face revision and checked against their SHA-256. The image is about 5.7 GB. A worker offers it only if it gives a container at least 3 GB of memory.
- **Its tasks run as Chronos-Bolt's do**: no network, one CPU, one thread, up to 3 GB, all of a task's fits in one container, one horizon at a time in batches of up to 64, each forecast from at most the series' last 1,024 points.
- **Its forecasts are checked within the same tolerance**, one part in a thousand of the series' size. On one machine it gives the same bits on a rerun, on one thread or four, and in batches of 1 or 64 (102 M4 series). How far apart honest results are across processors is not yet measured.

### On M4

Every tenth M4 series (10,001), one `forecasts` job per frequency, the models written in Go alone, then with `chronos-base`, then with `timesfm-200m` ([#683](https://github.com/sisyphus-network/Sisyphus/issues/683)). OWA:

| Frequency | Series | Go only | With chronos-base | With timesfm-200m |
| --- | ---: | ---: | ---: | ---: |
| Yearly | 2,300 | 0.803 | 0.802 | 0.802 |
| Quarterly | 2,400 | 0.918 | 0.911 | 0.911 |
| Monthly | 4,800 | 0.914 | 0.902 | 0.898 |
| Weekly | 36 | 0.902 | 0.843 | 0.836 |
| Daily | 423 | 1.000 | 0.993 | 1.000 |
| Hourly | 42 | 0.620 | 0.522 | 0.515 |
| **Overall** | **10,001** | **0.872** | **0.865** | **0.863** |

Overall sMAPE was 12.352, 12.240 and 12.224, MASE 1.591, 1.579 and 1.577, and MSIS 16.095, 15.910 and 15.844; no candidate failed. With eight tasks at a time on one machine, the three runs took 15 seconds, 1 h 28 min and 6 h 27 min. Continuous-integration jobs shared the machine for most of the TimesFM run, so its time is an upper bound, and most of it went on yearly and quarterly series, where each task's time is likely spent loading the model rather than forecasting. So TimesFM is a small gain over Chronos-Bolt here for several times its cost, and it is offered as a choice, not a default. M4 may be in both models' training data, so these numbers may flatter both.
