import { useEffect, useRef, useState } from 'react'

export function useStickyState(offset = 0) {
  const sentinelRef = useRef<HTMLSpanElement>(null)
  const [stuck, setStuck] = useState(false)
  useEffect(() => {
    const sentinel = sentinelRef.current
    if (!sentinel) return
    let root: HTMLElement | null = sentinel.parentElement
    while (root && !/(auto|scroll)/.test(getComputedStyle(root).overflowY)) root = root.parentElement
    const observer = new IntersectionObserver(([entry]) => {
      if (entry) setStuck(!entry.isIntersecting && entry.boundingClientRect.top < (entry.rootBounds?.top ?? offset))
    }, { root, rootMargin: `-${offset}px 0px 0px 0px`, threshold: 0 })
    observer.observe(sentinel)
    return () => observer.disconnect()
  }, [offset])
  return { sentinelRef, stuck }
}
