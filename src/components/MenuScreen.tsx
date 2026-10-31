"use client";

import type { CSSProperties } from "react";
import { cx } from "@/lib/cx";
import { DIFFICULTIES, DIFFICULTY_BLURB, DIFFICULTY_LABEL } from "@/lib/game/constants";
import type { Difficulty } from "@/lib/game/types";
import { CardFace } from "./PlayingCard";
import type { TierRecord } from "./ResultPanel";

interface MenuScreenProps {
  difficulty: Difficulty;
  stats: Partial<Record<Difficulty, TierRecord>>;
  soundOn: boolean;
  onDifficulty: (d: Difficulty) => void;
  onStart: () => void;
  onRules: () => void;
  onToggleSound: () => void;
}

/** A fanned showpiece hand for the menu: a run, a set and a stray card. */
const SHOWCASE = [26 + 4, 26 + 5, 26 + 6, 12, 25, 38, 13 + 0] as const;

export function MenuScreen({
  difficulty,
  stats,
  soundOn,
  onDifficulty,
  onStart,
  onRules,
  onToggleSound,
}: MenuScreenProps) {
  return (
    <>
      <h1 className="gr-title">{"🃏 GIN RUMMY"}</h1>
      <p className="gr-subtitle">Meld, knock and go gin against a card-counting CPU.</p>

      <div className="gr-showcase" aria-hidden>
        {SHOWCASE.map((c, i) => (
          <span key={c} className="gr-showcase-card" style={{ "--i": i - 3 } as CSSProperties}>
            <CardFace card={c} meld={i < 3 ? 0 : i < 6 ? 1 : -1} dead={i === 6} />
          </span>
        ))}
      </div>

      <div className="gr-panel">
        <span className="gr-field-label">Opponent</span>
        <div className="gr-choice-row">
          {DIFFICULTIES.map((d) => (
            <button
              key={d}
              type="button"
              className={cx("gr-choice", d === difficulty && "is-on")}
              onClick={() => onDifficulty(d)}
              aria-pressed={d === difficulty}
            >
              {DIFFICULTY_LABEL[d]}
            </button>
          ))}
        </div>
        <p className="gr-blurb">{DIFFICULTY_BLURB[difficulty]}</p>

        <button type="button" className="gr-primary" onClick={onStart}>
          Deal me in
        </button>

        <div className="gr-stats-grid">
          {DIFFICULTIES.map((d) => {
            const s = stats[d];
            return (
              <div key={d} className={cx("gr-statbox", d === difficulty && "is-on")}>
                <span className="gr-statbox-label">{DIFFICULTY_LABEL[d]}</span>
                <span className="gr-statbox-value">{s ? `${s.won}–${s.lost}` : "—"}</span>
                <span className="gr-statbox-sub">{s && s.best > 0 ? `best +${s.best}` : "won–lost"}</span>
              </div>
            );
          })}
        </div>

        <div className="gr-menu-row">
          <button type="button" className="gr-ghost" onClick={onRules}>
            How to play
          </button>
          <button type="button" className="gr-ghost" onClick={onToggleSound} aria-pressed={soundOn}>
            {soundOn ? "🔊 Sound on" : "🔇 Sound off"}
          </button>
        </div>
      </div>
    </>
  );
}
