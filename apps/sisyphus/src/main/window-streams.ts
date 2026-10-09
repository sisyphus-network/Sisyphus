type Stream = { cancel: () => void }

export class WindowStreams {
  private owners = new Map<number, Map<string, Stream>>()

  add(owner: number, id: string, stream: Stream) {
    this.stop(owner, id)
    const streams = this.owners.get(owner) ?? new Map<string, Stream>()
    streams.set(id, stream)
    this.owners.set(owner, streams)
  }

  has(owner: number, id: string, stream: Stream) {
    return this.owners.get(owner)?.get(id) === stream
  }

  release(owner: number, id: string, stream: Stream) {
    if (!this.has(owner, id, stream)) return
    const streams = this.owners.get(owner)!
    streams.delete(id)
    if (!streams.size) this.owners.delete(owner)
  }

  stop(owner: number, id: string) {
    const stream = this.owners.get(owner)?.get(id)
    if (!stream) return
    this.release(owner, id, stream)
    stream.cancel()
  }

  clear(owner: number) {
    const streams = this.owners.get(owner)
    this.owners.delete(owner)
    for (const stream of streams?.values() ?? []) stream.cancel()
  }

  clearAll() {
    for (const owner of [...this.owners.keys()]) this.clear(owner)
  }
}
