"use client";

import type { CSSProperties } from "react";
import { cx } from "@/lib/cx";
import { backKey } from "@/lib/game/session";
import { CardBack } from "./PlayingCard";

interface OpponentHandProps {
  count: number;
  label: string;
  thinking: boolean;
  /** What the CPU did last, shown under its fan. */
  lastMove: string | null;
}

/**
 * The CPU's face-down fan. Slots are keyed by position, never by card, so the
 * page itself holds nothing about the hidden hand.
 */
export function OpponentHand({ count, label, thinking, lastMove }: OpponentHandProps) {
  return (
    <section className="gr-opp" aria-label={`${label}, ${count} cards`}>
      <div className="gr-opp-head">
        <span className="gr-seat-name">{label}</span>
        <span className={cx("gr-thinking", thinking && "is-on")} aria-hidden>
          <i />
          <i />
          <i />
        </span>
      </div>
      <div className="gr-opp-fan" style={{ "--n": count } as CSSProperties}>
        {Array.from({ length: count }, (_, i) => (
          <div key={i} className="gr-slot gr-opp-slot" data-flip={backKey(i)}>
            <CardBack />
          </div>
        ))}
      </div>
      <p className="gr-opp-move" aria-live="polite">
        {thinking ? "Thinking…" : (lastMove ?? " ")}
      </p>
    </section>
  );
}
