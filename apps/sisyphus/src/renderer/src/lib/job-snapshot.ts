export type JobSnapshotToken = { request: number; revision: number }

// A list response is only a fallback for the live job stream. Ignore it when
// a newer list request or a stream/submit update has already changed the jobs.
export function createJobSnapshotGate() {
  let request = 0
  let revision = 0

  return {
    begin(): JobSnapshotToken {
      request += 1
      return { request, revision }
    },
    changed() {
      revision += 1
    },
    accept(token: JobSnapshotToken) {
      if (token.request !== request || token.revision !== revision) return false
      revision += 1
      return true
    },
  }
}
