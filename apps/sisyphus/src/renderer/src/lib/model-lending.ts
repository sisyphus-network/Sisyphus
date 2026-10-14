export type ModelLendingValues = {
  enabled: boolean
  ollamaUrl: string
  only: string[]
  gpuLayers: number
  threads: number
  context: number
  keepAlive: string
}

/** Compare settings by meaning, not by Protobuf message metadata or key order. */
export function isModelLendingDirty(saved: ModelLendingValues | undefined, current: ModelLendingValues): boolean {
  if (!saved) return false
  return saved.enabled !== current.enabled
    || saved.ollamaUrl !== current.ollamaUrl
    || saved.gpuLayers !== current.gpuLayers
    || saved.threads !== current.threads
    || saved.context !== current.context
    || saved.keepAlive !== current.keepAlive
    || saved.only.length !== current.only.length
    || saved.only.some((model, index) => model !== current.only[index])
}
