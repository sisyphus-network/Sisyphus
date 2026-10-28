import { useEffect, useState } from 'react'
import { SharedStateDomain } from '@sisyphus/client'
import { getNodeApi } from '@/lib/node-api'
import { sharedStateDomainCode } from '@/lib/shared-state-domain'

export type StoredFile = { cid: string; name: string; sizeBytes: string | number; private: boolean; storedAtMs: string | number }

type SharedFilesEvent = {
  domain: number | string
  error?: string
  snapshot?: boolean
  state?: { case?: string; value?: { files?: StoredFile[] } } | string
  files?: { files?: StoredFile[] }
}

function normalizeFiles(files: StoredFile[] = []): StoredFile[] {
  return files.map((file) => ({
    cid: file.cid ?? '',
    name: file.name || file.cid || '',
    sizeBytes: file.sizeBytes ?? 0,
    private: file.private ?? false,
    storedAtMs: file.storedAtMs ?? 0,
  }))
}

export function useSharedFiles(): StoredFile[] {
  const [files, setFiles] = useState<StoredFile[]>([])

  useEffect(() => {
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    let receivedSnapshot = false
    let fallbackRequested = false
    const loadFallback = async () => {
      if (!active || receivedSnapshot || fallbackRequested) return
      fallbackRequested = true
      try {
        const result = await getNodeApi().call<{ files?: StoredFile[] }>('listFiles')
        if (active && !receivedSnapshot) setFiles(normalizeFiles(result.files))
      } catch { /* The file list is a shortcut; file uploads still work without it. */ }
    }
    const reconnect = () => {
      cancel = undefined
      void loadFallback()
      if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
    }
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<SharedFilesEvent>('watchSharedState', {
        domains: [SharedStateDomain.FILES],
      }, (event) => {
        if (sharedStateDomainCode(event.domain) !== 'files' || event.error) return
        const state = event.state && typeof event.state === 'object' && event.state.case === 'files'
          ? event.state.value
          : event.files
        if (!state) return
        receivedSnapshot = true
        if (!event.snapshot) attempt = 0
        setFiles(normalizeFiles(state.files))
      }, reconnect, reconnect)
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [])

  return files
}
