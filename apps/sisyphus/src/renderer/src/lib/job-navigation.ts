export function getJobHref(jobId: string): string {
  return `/operations?${new URLSearchParams({ job: jobId })}`
}

export function getSelectedJobId(search: string): string {
  return new URLSearchParams(search).get('job') ?? ''
}

// Prefer live list data; keep a directly loaded job available when the list
// does not contain it. A response for an earlier link cannot enter this view.
export function includeLinkedJob<T extends { jobId: string }>(jobs: T[], linked: T | undefined, selectedId: string): T[] {
  if (!linked || linked.jobId !== selectedId || jobs.some((job) => job.jobId === selectedId)) return jobs
  return [linked, ...jobs]
}
