import { Download } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { getNodeApi } from '@/lib/node-api'
import { formatJobResult } from '@/lib/job-result'
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
    link.download = `${job.jobId}-result`
    link.click()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  return <details open={open} className="mt-3 rounded-lg border border-[var(--app-line)] p-3">
    <summary className="cursor-pointer text-xs font-medium">{labels.jobResult} · {prettyBytes(result.byteLength)}</summary>
    {text === null ? <p className="mt-2 text-xs text-muted-foreground">{labels.binaryResult}</p> : <pre dir="auto" className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-words text-xs">{text.slice(0, 65536)}</pre>}
    {text && text.length > 65536 && <p className="mt-2 text-xs text-muted-foreground">{labels.resultTruncated}</p>}
    <Button type="button" variant="outline" size="sm" className="mt-2" onClick={download}><Download className="me-1 size-3" />{labels.download}</Button>
  </details>
}

async function downloadOutput(cid: string, jobId: string, index: number, messages: Messages) {
  try {
    const data = await getNodeApi().fetchFile(cid)
    const bytes = new Uint8Array(data.byteLength); bytes.set(data)
    const url = URL.createObjectURL(new Blob([bytes.buffer]))
    const link = document.createElement('a'); link.href = url; link.download = `${jobId}-output-${index + 1}`; link.click(); URL.revokeObjectURL(url)
  } catch (error) { toast.error(messages.operationText.fetchOutputFailed, { description: error instanceof Error ? error.message : String(error) }) }
}

// JobOutputs offers each file a finished job wrote to save.
export function JobOutputs({ job, messages }: { job: FinishedJob; messages: Messages }) {
  if (!job.outputBlobs?.length) return null
  return <div className="mt-2 flex flex-wrap gap-2">{job.outputBlobs.map((cid, index) => <Button key={cid} type="button" size="sm" variant="outline" onClick={() => void downloadOutput(cid, job.jobId, index, messages)}><Download className="me-1 size-3" />{messages.output} {index + 1}</Button>)}</div>
}
