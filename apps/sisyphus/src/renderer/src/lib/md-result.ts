// An md job's result, as the Run page shows it: what was simulated, and
// each replica's seed, speed and final energy.

export type MdReplica = { index: number; seed: number; platform: string; nsPerDay: number; energy: number }
export type MdSummary = { replicas: number; atoms: number; picoseconds: number; meanEnergy: number; spread: number; runs: MdReplica[] }

const number = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) ? value : Number.NaN)

// parseMdResult reads an md job's JSON result, or returns null for
// anything that is not one, so the page can fall back to showing it raw.
export function parseMdResult(result?: Uint8Array): MdSummary | null {
  if (!result?.byteLength) return null
  let value: Record<string, unknown>
  try { value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(result)) } catch { return null }
  if (!value || typeof value !== 'object' || !Array.isArray(value.runs)) return null
  const runs = value.runs.map((run: Record<string, unknown>, position: number) => ({
    index: Number.isInteger(run?.index) ? run.index as number : position,
    seed: number(run?.seed), platform: typeof run?.platform === 'string' ? run.platform : '',
    nsPerDay: number(run?.ns_per_day), energy: number(run?.potential_energy_kj_mol),
  })).sort((a: MdReplica, b: MdReplica) => a.index - b.index)
  const summary = {
    replicas: number(value.replicas), atoms: number(value.atoms), picoseconds: number(value.picoseconds_each),
    meanEnergy: number(value.mean_potential_energy_kj_mol), spread: number(value.potential_energy_stdev_kj_mol), runs,
  }
  return Number.isFinite(summary.replicas) && Number.isFinite(summary.meanEnergy) ? summary : null
}

// formatNumber writes a number for people, to the digits given, or a dash
// for one the result did not have.
export function formatNumber(value: number, digits: number, locale?: string): string {
  return Number.isFinite(value) ? value.toLocaleString(locale, { minimumFractionDigits: digits, maximumFractionDigits: digits }) : '—'
}
