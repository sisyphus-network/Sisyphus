import { useEffect, useState } from "react";
import { Toaster } from "@/components/ui/sonner";

export function ToasterResponsive() {
  const [mobile, setMobile] = useState(false);
  const [direction, setDirection] = useState<"ltr" | "rtl">("ltr");

  useEffect(() => {
    const media = window.matchMedia("(max-width: 639px)");
    const update = () => setMobile(media.matches);
    media.addEventListener("change", update);
    update();
    return () => media.removeEventListener("change", update);
  }, []);

  useEffect(() => {
    const updateDirection = () => setDirection(document.documentElement.dir === "rtl" ? "rtl" : "ltr");
    const observer = new MutationObserver(updateDirection);
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ["dir"] });
    updateDirection();
    return () => observer.disconnect();
  }, []);

  return <Toaster key={`${direction}-${mobile ? "mobile" : "desktop"}`} dir={direction} richColors closeButton={!mobile} position={mobile ? "top-center" : "bottom-center"} />;
}
