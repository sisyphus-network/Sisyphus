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
      const estimatedHeight = contentRef.current?.getBoundingClientRect().height ?? 250;
      const spaceBelow = window.innerHeight - rect.bottom;
      const nextPlacement = spaceBelow < estimatedHeight && rect.top > estimatedHeight ? "top" : "bottom";
      const left = Math.max(8, Math.min(rect.left, window.innerWidth - rect.width - 8));
      const top = nextPlacement === "top"
        ? Math.max(8, rect.top - estimatedHeight - 8)
        : Math.min(window.innerHeight - 8, rect.bottom + 8);
      setPlacement(nextPlacement);
      setFloatingStyle({ position: "fixed", top, left, width: rect.width, maxHeight: "calc(100dvh - 16px)" });
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
