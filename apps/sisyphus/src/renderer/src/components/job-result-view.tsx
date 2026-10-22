import { Download, FileCode2 } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { getNodeApi } from '@/lib/node-api'
import { formatJobResult } from '@/lib/job-result'
import { nameInResult, outputFileName, resultFileName } from '@/lib/output-names'
import { type getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
type FinishedJob = { jobId: string; result?: Uint8Array; outputBlobs?: string[] }

export const prettyBytes = (value: string | number) => { const bytes = Number(value); if (!bytes) return '0 B'; const unit = Math.min(3, Math.floor(Math.log(bytes) / Math.log(1024))); return `${(bytes / 1024 ** unit).toFixed(unit ? 1 : 0)} ${['B', 'KB', 'MB', 'GB'][unit]}` }

// JobResult shows what a finished job returned, as text where it is text,
// and offers it to save.
export function JobResult({ job, messages, open = false }: { job: FinishedJob; messages: Messages; open?: boolean }) {
  if (!job.result?.byteLength) return null
  const result = job.result
  const text = formatJobResult(result)
  const labels = messages.operationText
  const download = () => {
    const url = URL.createObjectURL(new Blob([new Uint8Array(result).buffer]))
    const link = document.createElement('a')
    link.href = url
    link.download = resultFileName(job)
    link.click()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  return <Collapsible defaultOpen={open} className="overflow-hidden rounded-2xl border border-[var(--app-line)] bg-[var(--app-card)]">
    <CollapsibleTrigger title={labels.jobResult} icon={FileCode2} rightSlot={<span dir="ltr" className="rounded-full bg-[var(--app-wash)] px-2.5 py-1 text-xs tabular-nums text-muted-foreground">{prettyBytes(result.byteLength)}</span>} className="rounded-none px-3 py-3 sm:px-4" />
    <CollapsibleContent animateSpacing={false}>
      <div className="space-y-3 border-t border-[var(--app-line)] p-3 sm:p-4">
        {text === null ? <p className="rounded-xl bg-[var(--app-wash)] p-4 text-sm text-muted-foreground">{labels.binaryResult}</p> : <pre dir="auto" tabIndex={0} className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-xl bg-[var(--app-wash)] p-4 font-mono text-xs leading-6 text-[var(--app-ink)] focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]">{text.slice(0, 65536)}</pre>}
        {text && text.length > 65536 && <p className="text-xs leading-relaxed text-muted-foreground">{labels.resultTruncated}</p>}
        <div className="flex justify-end"><Button type="button" variant="outline" size="sm" className="gap-2 rounded-full" onClick={download}><Download className="size-3.5" />{labels.download}</Button></div>
      </div>
    </CollapsibleContent>
  </Collapsible>
}

async function downloadOutput(cid: string, job: FinishedJob, index: number, messages: Messages) {
  try {
    const data = await getNodeApi().fetchFile(cid)
    const bytes = new Uint8Array(data.byteLength); bytes.set(data)
    const url = URL.createObjectURL(new Blob([bytes.buffer]))
    const link = document.createElement('a'); link.href = url; link.download = outputFileName(job, cid, index, bytes); link.click(); URL.revokeObjectURL(url)
  } catch (error) { toast.error(messages.operationText.fetchOutputFailed, { description: error instanceof Error ? error.message : String(error) }) }
}

// JobOutputs offers each file a finished job wrote to save.
export function JobOutputs({ job, messages }: { job: FinishedJob; messages: Messages }) {
  if (!job.outputBlobs?.length) return null
  return <div className="mt-2 flex flex-wrap gap-2">{job.outputBlobs.map((cid, index) => <Button key={cid} type="button" size="sm" variant="outline" onClick={() => void downloadOutput(cid, job, index, messages)}><Download className="me-1 size-3" /><bdi>{nameInResult(job.result, cid) ?? `${messages.output} ${index + 1}`}</bdi></Button>)}</div>
}
