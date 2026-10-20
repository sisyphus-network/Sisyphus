type Job = { state?: string | number }
type Worker = { peerId?: string }

const activeJobStates = new Set<string | number>(['JOB_STATE_PENDING', 'JOB_STATE_RUNNING', 1, 2])

/**
 * What a restart of this node would interrupt: the jobs it has not finished
 * and the other machines working for it. Its own worker is not counted, as
 * it restarts with the node.
 */
export function restartImpact(jobs: Job[], workers: Worker[], self: string | undefined): { jobs: number; workers: number } {
  return {
    jobs: jobs.filter((job) => job.state !== undefined && activeJobStates.has(job.state)).length,
    workers: workers.filter((worker) => worker.peerId && worker.peerId !== self).length,
  }
}
