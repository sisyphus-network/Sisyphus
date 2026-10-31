import { AssetImage } from '@/components/ui/asset-image'

// Bundle the package's flags as Vite-managed URLs, including for Electron's
// file:// build. Use the actual asset list rather than assuming every pair of
// letters represents a supported flag.
const flagAssets = import.meta.glob<string>('../../../../../node_modules/flagpack-core/svg/m/*.svg', { eager: true, query: '?url', import: 'default' })
const flagSources = new Map(Object.entries(flagAssets).map(([path, url]) => [path.slice(path.lastIndexOf('/') + 1, -4), url]))
// Flagpack names the United Kingdom asset GB-UKM; the protocol uses ISO GB.
const assetCode = (code: string) => code.toUpperCase() === 'GB' ? 'GB-UKM' : code.toUpperCase()

export function hasCountryFlag(code?: string): boolean {
  return typeof code === 'string' && flagSources.has(assetCode(code))
}

export function CountryFlag({ code, size = 18, className }: { code?: string; size?: number; className?: string }) {
  const src = code && flagSources.get(assetCode(code))
  if (!src) return null
  return <AssetImage src={src} alt="" aria-hidden="true" width={size} height={size} containerClassName={className} className="rounded-full object-cover" />
}
