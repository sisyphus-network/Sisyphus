import { createContext, useState } from 'react'

export const NodeSettingsSearchContext = createContext('')

// Search expansion is separate from the user's normal disclosure state.
// Clearing the query restores the layout, without remounting any forms.
export function useSettingsSearchExpansion(query: string, defaultOpen = false) {
  const [manualOpen, setManualOpen] = useState(defaultOpen)
  const [searchState, setSearchState] = useState({ query: '', open: true })
  const open = query ? searchState.query === query ? searchState.open : true : manualOpen
  const onOpenChange = (next: boolean) => {
    if (query) setSearchState({ query, open: next })
    else setManualOpen(next)
  }
  return { open, onOpenChange }
}
