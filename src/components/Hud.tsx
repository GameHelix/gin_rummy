"use client";

import type { CSSProperties } from "react";
import { cx } from "@/lib/cx";

interface HudProps {
  names: readonly [string, string];
  scores: readonly [number, number];
  target: number;
  dealer: 0 | 1;
  handNo: number;
  onScores: () => void;
}

function ScoreStat({
  name,
  score,
  target,
  dealer,
  side,
}: {
  name: string;
  score: number;
  target: number;
  dealer: boolean;
  side: "me" | "cpu";
}) {
  const pct = Math.min(100, Math.round((score / target) * 100));
  return (
    <div className={cx("gr-stat", `is-${side}`)}>
      <span className="gr-stat-label">
        {name}
        {dealer ? (
          <span className="gr-dealer" title="Dealer" aria-label="dealer">
            D
          </span>
        ) : null}
      </span>
      <span className="gr-stat-value">{score}</span>
      <span className="gr-bar" aria-hidden>
        <span style={{ "--pct": `${pct}%` } as CSSProperties} />
      </span>
    </div>
  );
}

/** Running score to the target, the hand number and the dealer marker. */
export function Hud({ names, scores, target, dealer, handNo, onScores }: HudProps) {
  return (
    <div className="gr-hud">
      <ScoreStat name={names[0]} score={scores[0]} target={target} dealer={dealer === 0} side="me" />
      <button
        type="button"
        className="gr-stat gr-stat-mid"
        onClick={onScores}
        aria-label={`Hand ${handNo}. Open the scoreboard`}
      >
        <span className="gr-stat-label">Hand</span>
        <span className="gr-stat-value">{handNo}</span>
        <span className="gr-stat-sub">{`to ${target} ▸`}</span>
      </button>
      <ScoreStat name={names[1]} score={scores[1]} target={target} dealer={dealer === 1} side="cpu" />
    </div>
  );
}
