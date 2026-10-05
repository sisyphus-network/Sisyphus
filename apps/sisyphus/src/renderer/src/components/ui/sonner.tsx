"use client";

import { useEffect, useState } from "react";
import { CircleCheck, CircleX, Info, LoaderCircle, TriangleAlert } from "lucide-react";
import { Toaster as Sonner, type ToasterProps } from "sonner";

type ResolvedTheme = "light" | "dark";

function getTheme(): ResolvedTheme {
  if (document.documentElement.dataset.theme === "dark") return "dark";
  if (document.documentElement.dataset.theme === "light") return "light";
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function Toaster(props: ToasterProps) {
  const [theme, setTheme] = useState<ResolvedTheme>("light");

  useEffect(() => {
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    const update = () => setTheme(getTheme());
    const observer = new MutationObserver(update);
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    media.addEventListener("change", update);
    update();
    return () => {
      observer.disconnect();
      media.removeEventListener("change", update);
    };
  }, []);

  return <Sonner theme={theme} className="app-toaster" icons={{ success: <CircleCheck className="size-4" />, info: <Info className="size-4" />, warning: <TriangleAlert className="size-4" />, error: <CircleX className="size-4" />, loading: <LoaderCircle className="size-4 animate-spin" /> }} toastOptions={{ classNames: { toast: "app-toast", title: "app-toast__title", description: "app-toast__description", icon: "app-toast__icon", closeButton: "app-toast__close", actionButton: "app-toast__action", cancelButton: "app-toast__cancel" } }} {...props} />;
}
