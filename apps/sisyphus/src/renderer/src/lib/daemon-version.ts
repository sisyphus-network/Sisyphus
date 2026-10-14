/** Add the conventional v prefix only to semantic versions, not build labels such as dev/test-<commit>. */
export function displayDaemonVersion(version: string): string {
  if (/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.test(version)) return `v${version}`
  return version
}
