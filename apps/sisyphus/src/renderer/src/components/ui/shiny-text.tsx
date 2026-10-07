"use client";

import { useEffect, useRef } from "react";
import { motion, useAnimationFrame, useMotionValue, useTransform } from "motion/react";

type ShinyTextProps = {
  text: string;
  className?: string;
  speed?: number;
  delay?: number;
  color?: string;
  shineColor?: string;
  spread?: number;
  direction?: "left" | "right";
};

export default function ShinyText({
  text,
  className = "",
  speed = 2,
  delay = 0,
  color = "var(--shiny-text-base, var(--app-muted, #71717a))",
  shineColor = "var(--shiny-text-shine, var(--app-ink, #18181b))",
  spread = 120,
  direction = "left",
}: ShinyTextProps) {
  const progress = useMotionValue(0);
  const elapsed = useRef(0);
  const lastTime = useRef<number | null>(null);
  const duration = speed * 1000;
  const pause = delay * 1000;

  useAnimationFrame((time) => {
    if (lastTime.current === null) {
      lastTime.current = time;
      return;
    }

    elapsed.current += time - lastTime.current;
    lastTime.current = time;
    const cycle = duration + pause;
    const position = elapsed.current % cycle;
    const value = position < duration ? (position / duration) * 100 : 100;
    progress.set(direction === "left" ? value : 100 - value);
  });

  useEffect(() => {
    elapsed.current = 0;
    lastTime.current = null;
    progress.set(0);
  }, [direction, progress]);

  const backgroundPosition = useTransform(progress, (value) => `${150 - value * 2}% center`);

  return (
    <motion.span
      className={`inline-block ${className}`}
      style={{
        backgroundImage: `linear-gradient(${spread}deg, ${color} 0%, ${color} 35%, ${shineColor} 50%, ${color} 65%, ${color} 100%)`,
        backgroundSize: "200% auto",
        backgroundPosition,
        WebkitBackgroundClip: "text",
        backgroundClip: "text",
        WebkitTextFillColor: "transparent",
      }}
    >
      {text}
    </motion.span>
  );
}
