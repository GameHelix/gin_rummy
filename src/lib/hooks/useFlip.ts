"use client";

import { useEffect, useLayoutEffect, useRef, type RefObject } from "react";

/** Which on-screen card a newly shown card should glide in from. */
export interface FlipCue {
  nonce: number;
  from: Record<string, string>;
  /** A fresh deal: every card flies out of the stock, staggered. */
  deal: boolean;
}

interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
}

const DURATION_MS = 420;
const DEAL_STAGGER_MS = 30;

function measure(root: HTMLElement, nodes: HTMLElement[]): Map<string, Box> {
  const origin = root.getBoundingClientRect();
  const out = new Map<string, Box>();
  for (const n of nodes) {
    const r = n.getBoundingClientRect();
    out.set(n.dataset.flip ?? "", {
      x: r.left - origin.left,
      y: r.top - origin.top,
      w: r.width,
      h: r.height,
    });
  }
  return out;
}

function reducedMotion(): boolean {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

/**
 * FLIP card motion with plain CSS transitions. Every animated element carries a
 * `data-flip` key. After each move, elements are measured in their new places;
 * any that moved are snapped back to where they were (or to the element named
 * in `cue.from`, for a card that just appeared — say, out of the stock) and
 * then released, so the browser glides them home.
 *
 * Positions are kept relative to `root`, so scrolling never reads as motion,
 * and motion is skipped entirely for `prefers-reduced-motion`.
 */
export function useFlip(root: RefObject<HTMLElement | null>, cue: FlipCue, layoutKey: string) {
  const boxes = useRef<Map<string, Box>>(new Map());
  const seen = useRef({ nonce: -1, layout: "" });

  useLayoutEffect(() => {
    const el = root.current;
    if (!el) return;
    const nodes = Array.from(el.querySelectorAll<HTMLElement>("[data-flip]"));
    // Settle anything still in flight so we measure true layout positions.
    for (const n of nodes) {
      n.style.transition = "none";
      n.style.transform = "";
    }
    const next = measure(el, nodes);
    const prev = boxes.current;
    boxes.current = next;

    const changed = seen.current.nonce !== cue.nonce || seen.current.layout !== layoutKey;
    seen.current = { nonce: cue.nonce, layout: layoutKey };
    if (!changed || reducedMotion()) return;

    const moving: { node: HTMLElement; delay: number }[] = [];
    let order = 0;
    for (const n of nodes) {
      const key = n.dataset.flip ?? "";
      const to = next.get(key);
      const via = cue.from[key];
      const from = cue.deal
        ? via
          ? (next.get(via) ?? prev.get(via))
          : undefined
        : (prev.get(key) ?? (via ? (prev.get(via) ?? next.get(via)) : undefined));
      if (!to || !from) continue;
      const dx = from.x - to.x;
      const dy = from.y - to.y;
      const sx = to.w > 0 ? from.w / to.w : 1;
      const sy = to.h > 0 ? from.h / to.h : 1;
      if (Math.abs(dx) < 0.5 && Math.abs(dy) < 0.5 && Math.abs(sx - 1) < 0.01) continue;
      n.style.transformOrigin = "0 0";
      n.style.transform = `translate(${dx}px, ${dy}px) scale(${sx}, ${sy})`;
      moving.push({ node: n, delay: cue.deal ? order++ * DEAL_STAGGER_MS : 0 });
    }
    if (moving.length === 0) return;
    // One forced reflow commits the inverted positions before releasing them.
    void el.offsetWidth;
    for (const { node, delay } of moving) {
      node.style.transition = `transform ${DURATION_MS}ms cubic-bezier(0.22, 0.8, 0.26, 1) ${delay}ms`;
      node.style.transform = "";
    }
  }, [root, cue, layoutKey]);

  // A resize moves everything without a game event: re-measure quietly so the
  // next move does not glide from stale places.
  useEffect(() => {
    const onResize = () => {
      const el = root.current;
      if (!el) return;
      boxes.current = measure(el, Array.from(el.querySelectorAll<HTMLElement>("[data-flip]")));
    };
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, [root]);
}
