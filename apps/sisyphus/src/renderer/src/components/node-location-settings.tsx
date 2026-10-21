import { useEffect, useState } from 'react'
import { LoaderCircle, MapPin, Save, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { getNodeApi } from '@/lib/node-api'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { formatCoordinates, locationFromProtocol, locationShares, parseCoordinates, shareToProtocol, type LocationShare, type NodeLocation } from '@/lib/node-location'
import type { getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>

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
  const dirty = coordinates !== null && (!saved || saved.share !== share || formatCoordinates(saved.latitude, saved.longitude) !== formatCoordinates(coordinates.latitude, coordinates.longitude))

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

  return <Card className="settings-card min-w-0">
    <CardContent className="p-4 sm:p-5">
      <div className="flex items-start gap-3">
        <div className="settings-row__icon"><MapPin className="size-4" /></div>
        <div className="min-w-0 flex-1"><h3 className="text-sm font-semibold">{labels.title}</h3><p className="mt-1 text-sm leading-relaxed text-muted-foreground">{labels.description}</p></div>
      </div>
      {loading ? <div role="status" className="flex items-center gap-2 py-7 text-sm text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />{messages.connecting}</div>
        : unavailable ? <p role="note" className="mt-4 text-sm text-muted-foreground">{labels.unavailable}</p>
        : <form onSubmit={(event) => { event.preventDefault(); if (coordinates && dirty) void store({ ...coordinates, share }) }}>
          <div className="settings-row settings-row--divider flex-wrap">
            <div className="min-w-0 flex-1"><label htmlFor="node-location-coordinates" className="text-sm font-medium">{labels.coordinatesLabel}</label><p id="node-location-coordinates-help" className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.coordinatesHelp}</p></div>
            <Input id="node-location-coordinates" dir="ltr" inputMode="decimal" autoComplete="off" spellCheck={false} maxLength={80} value={text} disabled={saving} onChange={(event) => setText(event.target.value)} placeholder={labels.coordinatesPlaceholder} aria-describedby="node-location-coordinates-help" aria-invalid={invalid} className="w-full max-w-sm text-left font-mono text-sm" />
          </div>
          {invalid && <p role="alert" className="text-xs text-destructive">{labels.coordinatesInvalid}</p>}
          <div className="settings-row settings-row--divider flex-wrap">
            <div className="min-w-0 flex-1"><label htmlFor="node-location-share" className="text-sm font-medium">{labels.shareLabel}</label><p id="node-location-share-help" className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.shareHelp}</p></div>
            <select id="node-location-share" value={share} disabled={saving} aria-describedby="node-location-share-help" onChange={(event) => setShare(event.target.value as LocationShare)} className="h-9 w-full max-w-sm rounded-lg border border-[var(--app-line)] bg-background px-3 text-sm">
              {locationShares.map((option) => <option key={option} value={option}>{shareLabel[option]}</option>)}
            </select>
          </div>
          <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
            <p role="status" className="text-xs leading-relaxed text-muted-foreground">{saved ? labels.pinned : labels.notPinned}</p>
            <div className="flex flex-wrap gap-2">
              {saved && <Button type="button" variant="outline" disabled={saving} onClick={() => void store(null)} className="gap-2"><Trash2 className="size-4" />{labels.remove}</Button>}
              <Button type="submit" disabled={!dirty || saving} className="gap-2"><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</Button>
            </div>
          </div>
        </form>}
    </CardContent>
  </Card>
}
