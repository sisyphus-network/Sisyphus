# How well the forecast workload forecasts

The `forecast` workload picks a model for a series by backtesting a range of candidates on the series' own past and forecasting with the one that did best. Whether that forecasts well is a question with a standard answer: the [M4 competition](https://github.com/Mcompetitions/M4-methods)'s 100,000 series, scored the way the competition scored its 61 entries. `packages/runtime/forecastbench` does that, so the workload's accuracy is measured rather than claimed, and so every change to it can be measured the same way.

## What is measured

Each M4 series comes in two parts: its past, which the forecast is made from, and the values that followed, which it is scored against. The series are yearly, quarterly, monthly, weekly, daily and hourly, each forecast as far ahead as the competition asked (6, 8, 18, 13, 14 and 48 steps), and the quarterly, monthly and hourly ones with their cycle (4, 12 and 24 steps).

The scores are the competition's:

- **sMAPE**, the symmetric mean absolute percentage error, in percent.
- **MASE**, the mean absolute error scaled by how far each value of the series' past is from the one a cycle before it.
- **OWA**, the overall weighted average: sMAPE and MASE each as a share of those of **Naive2**, the competition's benchmark, averaged. Naive2 carries the last value forward, after taking the cycle out of a series that has one and putting it back into the forecast. An OWA of 1 is as good as Naive2; lower is better.

Each series is forecast with the workload exactly as a job would be: its candidates are split into tasks, the tasks are run, and the results aggregated, in one process. A pool forecasts the same, since the answer does not depend on how the job is split. The backtest uses as many windows of the horizon, up to four, as leave the models at least a horizon's worth of points to be fitted to. A series the workload cannot forecast would be scored as Naive2 and counted under **fell back**; on M4 there are none.

Naive2 and the scores are written from the competition's own code. Measured here, Naive2 scores an sMAPE of 13.564 and a MASE of 1.912 over all the series, the competition's published figures for it, which is the check that the scoring is the competition's.

## Running it

The data is not in the repository. Download the twelve files from the [`Dataset`](https://github.com/Mcompetitions/M4-methods/tree/master/Dataset) directory of the competition's repository, `Train/Yearly-train.csv` to `Test/Hourly-test.csv`, into one directory, then:

```sh
SISYPHUS_M4=~/m4-data go test -run 'TestM4$' -v -timeout 1h ./packages/runtime/forecastbench
```

`SISYPHUS_M4_EVERY=100` scores every hundredth series instead, a thousand in all, for a quick look. All 100,000 take under a minute on a 32-core machine. Without `SISYPHUS_M4` the test is skipped, so the ordinary test runs never need the data.

## Results

All 100,000 series, with the workload as of [#634](https://github.com/sisyphus-network/Sisyphus/pull/634):

| Frequency | Series | sMAPE | MASE | OWA | Naive2 sMAPE | Naive2 MASE |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Yearly | 23,000 | 15.453 | 3.580 | 0.923 | 16.342 | 3.974 |
| Quarterly | 24,000 | 11.562 | 1.280 | 0.992 | 11.012 | 1.371 |
| Monthly | 48,000 | 15.177 | 1.048 | 1.019 | 14.427 | 1.063 |
| Weekly | 359 | 8.557 | 2.435 | 0.905 | 9.161 | 2.777 |
| Daily | 4,227 | 3.507 | 3.693 | 1.139 | 3.045 | 3.278 |
| Hourly | 414 | 17.957 | 1.012 | 0.700 | 18.383 | 2.395 |
| **Overall** | **100,000** | **13.867** | **1.803** | **0.983** | **13.564** | **1.912** |

For comparison, from the competition's results: the winner, Smyl's hybrid of exponential smoothing and a recurrent network, scored an OWA of 0.821 (sMAPE 11.374, MASE 1.536), and the best single method, Theta, 0.897 (sMAPE 12.309, MASE 1.696).

So today the workload is only slightly better than Naive2 overall, and worse than it on monthly and daily series. It does well where a series' cycle is long and regular (hourly, 0.700) and where trends carry on (yearly, 0.923). What is likely to help, each to be measured here before it is kept:

- **Choosing by a scale-free error.** The backtest ranks candidates by root mean square error, which a few large misses dominate; M4 scores by sMAPE and MASE.
- **Combining forecasts.** Averaging the best few candidates is the most consistent gain the competition found.
- **Taking the cycle out first**, as Naive2 and Theta do, rather than leaving it to Holt-Winters alone.
- **More models**: Theta and damped trends among them.
- **Foundation models** such as Chronos-Bolt and TimesFM, as container candidates in the same backtest, once their floating-point output can be verified within a tolerance ([#627](https://github.com/sisyphus-network/Sisyphus/issues/627), [#629](https://github.com/sisyphus-network/Sisyphus/pull/629)).
