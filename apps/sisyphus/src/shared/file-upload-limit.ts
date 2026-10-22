// The client materializes uploads in memory before sending them to the node.
// Keep its guard and the UI's advertised limit in sync.
export const MAX_CLIENT_FILE_UPLOAD_BYTES = 256 * 1024 * 1024

export function fileUploadLimitLabel(bytes = MAX_CLIENT_FILE_UPLOAD_BYTES): string {
  return `${new Intl.NumberFormat('en', { maximumFractionDigits: 1 }).format(bytes / (1024 * 1024))} MiB`
}
