// What the chat shows of a job the planner started while it runs: how far
// along it is, and what each worker is doing for it.

export type JobPhase = 'waiting' | 'running' | 'succeeded' | 'failed' | 'cancelled'

export type ProgressTask = { state: string | number; attempt?: number | string; workerName?: string }
export type ProgressJob = { state: string | number; tasks?: ProgressTask[] }

export type WorkerProgress = { name: string; running: number; done: number }

export type JobProgress = {
  phase: JobPhase
  total: number
  done: number
  running: number
  // Tasks handed to a worker more than once: a worker was lost, timed out or
  // failed, and the task was given out again.
  retried: number
  workers: WorkerProgress[]
}

const stateNames: Record<number, string> = { 1: 'pending', 2: 'running', 3: 'succeeded', 4: 'failed', 5: 'cancelled' }

// stateName reads a job or task state as the node API gives it, a number or
// a name such as JOB_STATE_RUNNING, as its short name.
export function stateName(state: string | number): string {
  if (typeof state === 'number') return stateNames[state] ?? 'unspecified'
  return state.replace(/^JOB_STATE_/, '').toLowerCase()
}

export function jobProgress(job: ProgressJob): JobProgress {
  const tasks = job.tasks ?? []
  const workers = new Map<string, WorkerProgress>()
  let done = 0
  let running = 0
  let retried = 0
  for (const task of tasks) {
    const state = stateName(task.state)
    if (state === 'succeeded') done++
    if (state === 'running') running++
    if (Number(task.attempt ?? 0) > 1) retried++
    const name = task.workerName?.trim()
    if (!name || (state !== 'running' && state !== 'succeeded')) continue
    const worker = workers.get(name) ?? { name, running: 0, done: 0 }
    if (state === 'running') worker.running++
    else worker.done++
    workers.set(name, worker)
  }
  const state = stateName(job.state)
  const phase: JobPhase = state === 'succeeded' || state === 'failed' || state === 'cancelled'
    ? state
    : running > 0 || done > 0 ? 'running' : 'waiting'
  return {
    phase, total: tasks.length, done, running, retried,
    workers: [...workers.values()].sort((a, b) => (b.done + b.running) - (a.done + a.running) || a.name.localeCompare(b.name)),
  }
}

// finished says whether a job is over, so that the chat can stop watching it.
export function finished(progress: JobProgress): boolean {
  return progress.phase === 'succeeded' || progress.phase === 'failed' || progress.phase === 'cancelled'
}
