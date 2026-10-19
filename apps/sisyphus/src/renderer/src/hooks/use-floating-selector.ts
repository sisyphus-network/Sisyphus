"use client";

import { useEffect, useRef, useState, type CSSProperties } from "react";

export function useFloatingSelector() {
  const rootRef = useRef<HTMLDivElement>(null);
  const contentRef = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [placement, setPlacement] = useState<"top" | "bottom">("bottom");
  const [floatingStyle, setFloatingStyle] = useState<CSSProperties>({});

  useEffect(() => {
    if (!open) return;

    const updatePlacement = () => {
      const rect = rootRef.current?.getBoundingClientRect();
      if (!rect) return;
      // The panel's natural height, not its capped one, so a panel that was
      // shrunk to fit is not taken to be small enough to fit anywhere.
      const content = contentRef.current;
      const estimatedHeight = content ? Math.max(content.scrollHeight, content.getBoundingClientRect().height) : 250;
      const spaceBelow = window.innerHeight - rect.bottom;
      const spaceAbove = rect.top;
      // Below if it fits there, or if below has the more room; then it gets
      // the room it has, and scrolls, rather than running off the window.
      const nextPlacement = spaceBelow - 16 >= estimatedHeight || spaceBelow >= spaceAbove ? "bottom" : "top";
      // Never let the preferred minimum exceed the actual available space:
      // on a very short visual viewport, staying inside it matters more than
      // keeping the usual 120px menu height.
      const availableSpace = nextPlacement === "top" ? spaceAbove : spaceBelow;
      const room = Math.max(0, availableSpace - 16);
      setPlacement(nextPlacement);
      // Base UI owns the coordinates. Keeping only a measured size constraint
      // here avoids competing with its flip/shift positioning calculations.
      setFloatingStyle({ maxHeight: room });
    };
    const handleOutsidePointer = (event: PointerEvent) => {
      const target = event.target as Node;
      if (rootRef.current?.contains(target) || contentRef.current?.contains(target)) return;
      setOpen(false);
    };

    updatePlacement();
    document.addEventListener("pointerdown", handleOutsidePointer, true);
    window.addEventListener("resize", updatePlacement);
    window.addEventListener("scroll", updatePlacement, true);
    window.visualViewport?.addEventListener("resize", updatePlacement);
    const observer = typeof ResizeObserver !== "undefined" ? new ResizeObserver(updatePlacement) : null;
    if (rootRef.current && observer) observer.observe(rootRef.current);
    if (contentRef.current && observer) observer.observe(contentRef.current);

    return () => {
      document.removeEventListener("pointerdown", handleOutsidePointer, true);
      window.removeEventListener("resize", updatePlacement);
      window.removeEventListener("scroll", updatePlacement, true);
      window.visualViewport?.removeEventListener("resize", updatePlacement);
      observer?.disconnect();
    };
  }, [open]);

  return { rootRef, contentRef, open, setOpen, placement, floatingStyle };
}
