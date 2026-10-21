// The workloads that run on a language model: every node takes their jobs
// in, to pass them on to a worker that serves a model, but only such a
// worker can carry them out (packages/runtime WithModels).
const modelWorkloads = new Set(['chat', 'embed', 'prompts'])

// offeredWorkloads is what a worker can actually run: its workloads, less
// the model ones when it serves no model.
export function offeredWorkloads(worker: { workloads?: string[]; models?: string[] }): string[] {
  const workloads = worker.workloads ?? []
  return worker.models?.length ? workloads : workloads.filter((workload) => !modelWorkloads.has(workload))
}
