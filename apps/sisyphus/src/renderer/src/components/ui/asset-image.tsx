"use client";

import { useCallback, useState, type ImgHTMLAttributes, type ReactNode } from "react";
import { cn } from "@/lib/utils";

type AssetImageProps = Omit<ImgHTMLAttributes<HTMLImageElement>, "children" | "content" | "onLoad" | "onError"> & {
  containerClassName?: string;
  imageClassName?: string;
  content?: ReactNode;
  contentClassName?: string;
};

export function AssetImage(props: AssetImageProps) {
  return <AssetImageContent key={String(props.src ?? "")} {...props} />;
}

function AssetImageContent({
  alt,
  className,
  containerClassName,
  imageClassName,
  content,
  contentClassName,
  src,
  width,
  height,
  ...props
}: AssetImageProps) {
  const [status, setStatus] = useState<"loading" | "loaded" | "error">("loading");
  const handleImageRef = useCallback((image: HTMLImageElement | null) => {
    if (!image?.complete) return;

    // Cached assets can finish before React attaches onLoad. Resolve their
    // state on the next frame so the placeholder never remains indefinitely.
    requestAnimationFrame(() => {
      if (!image.isConnected) return;
      setStatus(image.naturalWidth > 0 ? "loaded" : "error");
    });
  }, []);

  return (
    <span
      className={cn("relative isolate block", containerClassName)}
      style={typeof width === "number" && typeof height === "number" ? { width, height } : undefined}
      aria-busy={status === "loading"}
    >
      {status === "loading" ? <span aria-hidden className="asset-image-shimmer absolute inset-0" /> : null}
      {content ? <span className={cn("relative z-10 block size-full transition-opacity duration-300 ease-out motion-reduce:transition-none", status === "loaded" ? "opacity-100" : "opacity-0", contentClassName)}>{content}</span> : null}
      {/* Asset URLs may be local object URLs, so this intentionally avoids next/image optimization. */}
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img
        {...props}
        src={src}
        alt={alt}
        width={width}
        height={height}
        ref={handleImageRef}
        onLoad={() => setStatus("loaded")}
        onError={() => setStatus("error")}
        className={cn(
          content ? "pointer-events-none absolute inset-0 size-full opacity-0" : cn("relative block size-full transition-opacity duration-300 ease-out motion-reduce:transition-none", status === "loaded" ? "opacity-100" : "opacity-0"),
          className,
          imageClassName,
        )}
      />
      {status === "error" ? <span aria-hidden className="absolute inset-0 bg-[var(--app-wash)]" /> : null}
    </span>
  );
}
