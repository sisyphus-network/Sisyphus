import { SettingsSaveAction } from '@/components/ui/settings-save-dock'
import { SettingsSectionSkeleton } from '@/components/ui/settings-section-skeleton'
import { SettingsIcon } from '@/components/ui/settings-icon'
import { useEffect, useState } from 'react'
import { Building2, Crosshair, Globe2, LoaderCircle, MapPin, Radar, Save, ShieldCheck, ShieldOff, Trash2, type LucideIcon } from 'lucide-react'
import { toast } from 'sonner'
import { getNodeApi } from '@/lib/node-api'
import { Button } from '@/components/ui/button'
import { NodeSettingsSection } from '@/components/node-settings-section'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { SettingsSelector } from '@/components/ui/settings-selector'
import { formatCoordinates, locationFromProtocol, locationShares, parseCoordinates, shareToProtocol, type LocationShare, type NodeLocation } from '@/lib/node-location'
import type { getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
const shareIcons: Record<LocationShare, LucideIcon> = { exact: Crosshair, 'about-1km': Radar, city: Building2, country: Globe2, nothing: ShieldOff }

// NodeLocationSettingsCard lets the owner put this node where it is: the
// most precise of the node's location sources, and one that sends nothing
// anywhere to find it.
export function NodeLocationSettingsCard({ messages }: { messages: Messages }) {
  const labels = messages.nodeLocationSettings
  const [saved, setSaved] = useState<NodeLocation | null>(null)
  const [text, setText] = useState('')
  const [share, setShare] = useState<LocationShare>('country')
  const [loading, setLoading] = useState(true)
  const [unavailable, setUnavailable] = useState(false)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let active = true
    void getNodeApi().call<{ location?: unknown }>('getNodeLocation').then((result) => {
      if (!active) return
      const location = result.location ? locationFromProtocol(result.location) : null
      setSaved(location)
      if (location) {
        setText(formatCoordinates(location.latitude, location.longitude))
        setShare(location.share)
      }
    }).catch(() => { if (active) setUnavailable(true) })
      .finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [])

  const coordinates = parseCoordinates(text)
  const invalid = text.trim() !== '' && !coordinates
  const coordinatesChanged = saved ? !coordinates || formatCoordinates(saved.latitude, saved.longitude) !== formatCoordinates(coordinates.latitude, coordinates.longitude) : text.trim() !== ''
  const changeCount = Number(coordinatesChanged) + Number(share !== (saved?.share ?? 'country'))
  const dirty = changeCount > 0

  async function store(location: NodeLocation | null) {
    setSaving(true)
    try {
      const result = await getNodeApi().call<{ location?: unknown }>('setNodeLocation', location ? { location: { latitude: location.latitude, longitude: location.longitude, share: shareToProtocol(location.share) } } : {})
      const next = result.location ? locationFromProtocol(result.location) : null
      setSaved(next)
      if (next) {
        setText(formatCoordinates(next.latitude, next.longitude))
        setShare(next.share)
      } else {
        setText('')
        setShare('country')
      }
      toast.success(next ? labels.saved : labels.removed)
    } catch (error) {
      toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setSaving(false) }
  }

  const shareLabel: Record<LocationShare, string> = {
    exact: labels.shareExact, 'about-1km': labels.shareAbout1km, city: labels.shareCity, country: labels.shareCountry, nothing: labels.shareNothing,
  }

  return <NodeSettingsSection title={labels.title} description={labels.description} icon={MapPin}>
            {loading ? <SettingsSectionSkeleton label={messages.connecting} rows={2} />
        : unavailable ? <p role="note" className="mt-4 text-sm text-muted-foreground">{labels.unavailable}</p>
        : <form onSubmit={(event) => event.preventDefault()}>
          <div className="settings-row settings-row--divider flex-wrap">
            <div className="flex min-w-0 flex-1 items-start gap-3"><SettingsIcon icon={Crosshair} /><div className="min-w-0"><label htmlFor="node-location-coordinates" className="text-sm font-medium">{labels.coordinatesLabel}</label><p id="node-location-coordinates-help" className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.coordinatesHelp}</p></div></div>
            <FloatingLabelInput label={labels.coordinatesLabel} icon={<SettingsIcon icon={Crosshair} compact />} containerClassName="w-full sm:max-w-sm" id="node-location-coordinates" dir="ltr" inputMode="decimal" autoComplete="off" spellCheck={false} maxLength={80} value={text} disabled={saving} onChange={(event) => setText(event.target.value)} placeholder={labels.coordinatesPlaceholder} aria-describedby="node-location-coordinates-help" errorMessage={invalid ? labels.coordinatesInvalid : undefined} className="font-mono" />
          </div>
          <div className="settings-row settings-row--divider flex-wrap">
            <div className="flex min-w-0 flex-1 items-start gap-3"><SettingsIcon icon={ShieldCheck} /><div className="min-w-0"><label htmlFor="node-location-share" className="text-sm font-medium">{labels.shareLabel}</label><p id="node-location-share-help" className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.shareHelp}</p></div></div>
            <SettingsSelector id="node-location-share" label={labels.shareLabel} icon={<SettingsIcon icon={ShieldCheck} compact />} value={share} disabled={saving} onChange={(value) => setShare(value as LocationShare)} options={locationShares.map((value) => ({ value, label: shareLabel[value], icon: shareIcons[value] }))} className="w-full sm:max-w-sm" />
          </div>
          <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
            <p role="status" className="text-xs leading-relaxed text-muted-foreground">{saved ? labels.pinned : labels.notPinned}</p>
            <div className="flex flex-wrap gap-2">
              {saved && <Button type="button" variant="outline" disabled={saving} onClick={() => void store(null)} className="gap-2"><Trash2 className="size-4" />{labels.remove}</Button>}
              <SettingsSaveAction onRevert={() => { setText(saved ? formatCoordinates(saved.latitude, saved.longitude) : ''); setShare(saved?.share ?? 'country') }} dirty={dirty} changeCount={changeCount} disabled={!dirty || saving || !coordinates} onSave={async () => { if (coordinates) await store({ ...coordinates, share }) }}><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</SettingsSaveAction>
            </div>
          </div>
        </form>}
  </NodeSettingsSection>
}
