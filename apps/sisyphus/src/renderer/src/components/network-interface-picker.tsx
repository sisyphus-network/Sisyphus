import { useId, useMemo, useState, type Dispatch, type SetStateAction } from 'react'
import { Check, ChevronDown, EthernetPort, Plus, Search } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ResponsiveSelector } from '@/components/ui/responsive-selector'

export type NetworkInterfaceAddress = { name: string; address: string }

type NetworkInterfacePickerProps = {
  label: string
  placeholder: string
  searchPlaceholder: string
  emptyLabel: string
  customLabel: string
  doneLabel: string
  interfaces: NetworkInterfaceAddress[]
  value: string[]
  onChange: Dispatch<SetStateAction<string[]>>
  disabled?: boolean
}

export function NetworkInterfacePicker({ label, placeholder, searchPlaceholder, emptyLabel, customLabel, doneLabel, interfaces, value, onChange, disabled = false }: NetworkInterfacePickerProps) {
  const [query, setQuery] = useState('')
  const listboxId = useId()
  const options = useMemo(() => {
    const current = value.filter((address) => !interfaces.some((item) => item.address === address)).map((address) => ({ name: customLabel, address }))
    const unique = new Map<string, NetworkInterfaceAddress>()
    for (const item of [...current, ...interfaces]) if (!unique.has(item.address)) unique.set(item.address, item)
    return [...unique.values()]
  }, [customLabel, interfaces, value])
  const selected = value.map((address) => options.find((item) => item.address === address)).filter((item): item is NetworkInterfaceAddress => Boolean(item))
  const filtered = options.filter((item) => `${item.name} ${item.address}`.toLowerCase().includes(query.trim().toLowerCase()))
  const direction = document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'
  const searchDirection = contentDirection([query || searchPlaceholder])
  const toggle = (address: string) => onChange((current) => current.includes(address) ? current.filter((item) => item !== address) : [...current, address])

  return <ResponsiveSelector label={label} direction={direction} className="min-w-0" trigger={({ open, setOpen }) => <div className="relative min-w-0" dir={direction}>
    <Button type="button" variant="outline" role="combobox" aria-expanded={open} aria-haspopup="listbox" aria-controls={listboxId} aria-label={label} disabled={disabled} dir={direction} onClick={() => setOpen((current) => !current)} className="h-14 w-full justify-between gap-3 rounded-2xl border-[var(--app-border)] bg-[var(--app-card)] px-4 py-2.5 text-start font-normal hover:bg-[var(--app-wash)]">
      <span className="flex min-w-0 flex-1 items-center gap-3">
        <EthernetPort aria-hidden="true" className="size-[18px] shrink-0 text-[var(--app-muted)]" />
        {selected.length ? <span className="flex min-w-0 flex-1 items-center gap-1.5 overflow-hidden whitespace-nowrap">
          <span title={`${selected[0].name} · ${selected[0].address}`} className="flex min-w-0 max-w-[min(15rem,65%)] shrink-0 items-center gap-2">
            <span className="min-w-0 flex-1"><span dir={contentDirection([selected[0].name])} className="block truncate text-start text-sm font-medium leading-5">{selected[0].name}</span><span dir="ltr" className="block truncate text-start font-mono text-[10px] leading-4 text-[var(--app-muted)]">{selected[0].address}</span></span>
          </span>
          {selected.length > 1 && <><Plus aria-hidden="true" className="size-3.5 shrink-0 text-[var(--app-muted)]" /><span dir="ltr" onWheel={(event) => { const el = event.currentTarget; event.preventDefault(); event.stopPropagation(); const delta = Math.abs(event.deltaX) > Math.abs(event.deltaY) ? event.deltaX : event.deltaY; el.scrollBy({ left: delta, behavior: 'smooth' }) }} className="flex min-w-0 flex-1 items-center gap-1.5 overflow-x-auto overscroll-x-contain overscroll-y-none whitespace-nowrap scroll-smooth [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">{selected.slice(1).map((item) => <span key={item.address} title={`${item.name} · ${item.address}`} className="inline-flex max-w-32 shrink-0 items-center rounded-lg border border-[var(--app-accent)]/15 bg-[var(--app-accent)]/8 px-2 py-1 text-xs font-medium text-[var(--app-ink)]"><span dir="auto" className="block truncate">{item.name}</span></span>)}</span></>}
        </span> : <span className="block min-w-0 flex-1 truncate text-sm text-[var(--app-muted)]">{placeholder}</span>}
      </span>
      <ChevronDown aria-hidden="true" className="size-4 shrink-0 text-[var(--app-muted)]" />
    </Button>
    <span aria-hidden="true" className={`pointer-events-none absolute start-12 z-10 -translate-y-1/2 bg-[var(--app-card)] px-1.5 text-xs transition-[top,transform,opacity,color] duration-200 ease-out ${selected.length ? `top-0 scale-90 opacity-100 ${open ? 'text-[var(--app-accent)]' : 'text-[var(--app-muted)]'}` : 'top-1/2 scale-100 opacity-0'}`}>{label}</span>
  </div>}>
    {({ setOpen }) => <div className="min-w-[min(22rem,calc(100vw-2rem))]">
      <label dir={searchDirection} className="mb-2 flex h-10 items-center gap-2 rounded-xl border border-[var(--app-border)] bg-[var(--app-card)] px-3"><Search aria-hidden="true" className="size-4 shrink-0 text-[var(--app-muted)]" /><input type="search" role="searchbox" aria-label={searchPlaceholder} aria-controls={listboxId} dir={searchDirection} value={query} onChange={(event) => setQuery(event.target.value)} placeholder={searchPlaceholder} className="min-w-0 flex-1 bg-transparent text-start text-sm outline-none placeholder:text-[var(--app-muted)]" /></label>
      <div id={listboxId} dir="ltr" role="listbox" aria-label={label} aria-multiselectable="true" className="grid max-h-64 gap-1 overflow-y-auto overscroll-contain">{filtered.map((item) => <button type="button" role="option" aria-selected={value.includes(item.address)} key={`${item.name}:${item.address}`} dir={contentDirection([item.name])} onClick={() => toggle(item.address)} className="flex min-h-14 w-full items-center justify-between gap-3 rounded-xl px-3 py-2 text-start transition-colors hover:bg-[var(--app-wash)] aria-selected:bg-[var(--app-wash)]"><span className="min-w-0"><span dir={contentDirection([item.name])} className="block truncate text-start text-sm font-medium text-[var(--app-ink)]">{item.name}</span><span dir="ltr" className="mt-0.5 block truncate text-start font-mono text-[11px] text-[var(--app-muted)]">{item.address}</span></span>{value.includes(item.address) && <Check aria-hidden="true" className="size-4 shrink-0 text-[var(--app-accent)]" />}</button>)}</div>
      {!filtered.length && <p role="status" className="px-3 py-5 text-center text-sm text-[var(--app-muted)]">{emptyLabel}</p>}
      <div className="mt-2 flex justify-end border-t border-[var(--app-line)] pt-2"><Button type="button" variant="outline" size="sm" onClick={() => { setQuery(''); setOpen(false) }}>{doneLabel}</Button></div>
    </div>}
  </ResponsiveSelector>
}

function contentDirection(values: string[]): 'rtl' | 'ltr' {
  for (const value of values) for (const character of value) {
    if (/[\u0590-\u08ff\ufb1d-\ufdff\ufe70-\ufeff]/u.test(character)) return 'rtl'
    if (/[A-Za-z\u00c0-\u02af]/u.test(character)) return 'ltr'
  }
  return 'ltr'
}
