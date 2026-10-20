import { useEffect, useState } from 'react'
import { motion, useReducedMotion } from 'motion/react'
import { Cpu, RotateCcw } from 'lucide-react'
import { getNodeApi } from '@/lib/node-api'
import { finished, jobProgress, type JobProgress, type ProgressJob } from '@/lib/job-progress'
import { formatMessage, type getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
type WatchedJob = ProgressJob & { jobId: string }

function phaseLabel(progress: JobProgress, messages: Messages) {
  switch (progress.phase) {
    case 'waiting': return messages.jobTrackerWaiting
    case 'running': return messages.jobTrackerRunning
    case 'succeeded': return messages.jobTrackerSucceeded
    case 'failed': return messages.jobTrackerFailed
    case 'cancelled': return messages.jobTrackerCancelled
  }
}

// JobTracker shows a job the planner started as it runs: its tasks done and
// what each worker is doing, and stays as a summary once the job is over.
export function JobTracker({ jobId, messages }: { jobId: string; messages: Messages }) {
  const reduceMotion = useReducedMotion()
  const [progress, setProgress] = useState<JobProgress | null>(null)
  useEffect(() => {
    let over = false
    let stop: (() => void) | null = null
    stop = getNodeApi().stream<{ jobs?: WatchedJob[] }>('watchJobs', {}, (result) => {
      const job = result.jobs?.find((candidate) => candidate.jobId === jobId)
      if (!job) return
      const next = jobProgress(job)
      setProgress(next)
      if (finished(next) && !over) {
        over = true
        stop?.()
      }
    }, () => {})
    if (over) stop()
    return () => { over = true; stop?.() }
  }, [jobId])
  if (!progress || progress.total === 0) return null
  const share = progress.done / progress.total
  return <div role="group" aria-label={messages.jobTrackerLabel} className="mt-2 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] px-3 py-2.5 text-xs">
    <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
      <span className="font-semibold text-foreground">{phaseLabel(progress, messages)}</span>
      <span className="text-muted-foreground tabular-nums">
        {formatMessage(messages.jobTrackerTasks, { done: progress.done, total: progress.total })}
        {progress.retried > 0 && <span className="ms-2 inline-flex items-center gap-1 text-amber-600 dark:text-amber-400"><RotateCcw className="size-3" />{formatMessage(messages.jobTrackerRetried, { count: progress.retried })}</span>}
      </span>
    </div>
    <div role="progressbar" aria-label={messages.jobTrackerLabel} aria-valuemin={0} aria-valuemax={progress.total} aria-valuenow={progress.done} className="mt-2 h-1.5 overflow-hidden rounded-full bg-[var(--app-line)]">
      <motion.div className="h-full rounded-full bg-[var(--app-accent)]" initial={false} animate={{ width: `${Math.round(share * 100)}%` }} transition={reduceMotion ? { duration: 0 } : { duration: .4, ease: [.22, 1, .36, 1] }} />
    </div>
    {progress.workers.length > 0 && <ul className="mt-2 space-y-1">
      {progress.workers.map((worker) => <li key={worker.name} className="flex items-center gap-2 text-muted-foreground">
        <Cpu className={`size-3.5 shrink-0 ${worker.running > 0 && !reduceMotion ? 'animate-pulse text-[var(--app-accent)]' : ''}`} />
        <span className="min-w-0 truncate font-medium text-foreground">{worker.name}</span>
        <span className="ms-auto shrink-0 tabular-nums">{formatMessage(messages.jobTrackerWorker, { running: worker.running, done: worker.done })}</span>
      </li>)}
    </ul>}
  </div>
}
