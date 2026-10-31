"use client";

import { cx } from "@/lib/cx";
import { useFocusOnMount } from "@/lib/hooks/useFocusOnMount";
import { cardLabel } from "@/lib/game/cards";
import type { MatchScore } from "@/lib/game/match";
import type { Card, HandResult, Seat, SeatLayout } from "@/lib/game/types";
import { CardFace } from "./PlayingCard";
import { Scoreboard } from "./Scoreboard";

interface HandResultPanelProps {
  result: HandResult;
  score: MatchScore;
  names: readonly [string, string];
  onNext: () => void;
  onTally: () => void;
  onMenu: () => void;
}

const whose = (seat: Seat) => (seat === 0 ? "your" : "the CPU's");
const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;

function title(result: HandResult, names: readonly [string, string]): string {
  const k = result.knocker;
  switch (result.outcome) {
    case "knock":
      return `👊 ${names[k ?? 0]} knocked`;
    case "gin":
      return `✨ ${names[k ?? 0]} went gin!`;
    case "big-gin":
      return `💥 Big gin for ${k === 0 ? "you" : names[k ?? 1]}!`;
    case "undercut":
      return "🛡️ Undercut!";
    case "draw":
      return "🤝 Drawn hand";
  }
}

/** A seat as the subject of a sentence: "You" or "The CPU". */
const subject = (seat: Seat, start = false) =>
  seat === 0 ? (start ? "You" : "you") : start ? "The CPU" : "the CPU";

/** Why the points came out as they did, in a line or two. */
function explain(result: HandResult): string {
  const knocker = result.knocker;
  if (result.outcome === "draw" || knocker === null) {
    return "Only two cards were left in the stock and nobody knocked, so nobody scores.";
  }
  const defender: Seat = knocker === 0 ? 1 : 0;
  const k = result.layouts[knocker].deadwoodValue;
  const d = result.layouts[defender].deadwoodValue;
  const laid = result.layouts[defender].layoffs.length;
  const after = laid > 0 ? ` after laying off ${plural(laid, "card")}` : "";
  switch (result.outcome) {
    case "gin":
      return `25 for gin plus the ${d} deadwood left in ${whose(defender)} hand. No lay-offs against gin.`;
    case "big-gin":
      return `31 for big gin plus the ${d} deadwood left in ${whose(defender)} hand.`;
    case "knock":
      return `${subject(knocker, true)} knocked with ${k}; ${subject(defender)} held ${d}${after}. The difference scores.`;
    default:
      return `${subject(knocker, true)} knocked with ${k}, but ${subject(defender)} got down to ${d}${after}: the difference plus 25.`;
  }
}

function SeatTable({
  name,
  layout,
  knocked,
  incoming,
}: {
  name: string;
  layout: SeatLayout;
  knocked: boolean;
  /** Cards the defender laid off onto this seat's melds, by meld index. */
  incoming: Map<number, Card[]>;
}) {
  return (
    <section className="gr-seat">
      <header className="gr-seat-head">
        <span className="gr-seat-name">{name}</span>
        {knocked ? <span className="gr-tag">laid down</span> : null}
        <span className={cx("gr-seat-dw", layout.deadwoodValue === 0 && "is-zero")}>
          {`Deadwood ${layout.deadwoodValue}`}
        </span>
      </header>
      <div className="gr-groups">
        {layout.melds.map((m, i) => (
          <div key={`m${i}`} className={cx("gr-group", `gr-m${i % 4}`)}>
            {m.cards.map((c) => (
              <CardFace key={c} card={c} meld={i} mini />
            ))}
            {(incoming.get(i) ?? []).map((c) => (
              <CardFace key={c} card={c} meld={i} mini laidOff />
            ))}
          </div>
        ))}
        {layout.deadwood.length > 0 ? (
          <div className="gr-group is-dead">
            {layout.deadwood.map((c) => (
              <CardFace key={c} card={c} mini dead />
            ))}
          </div>
        ) : null}
      </div>
      {layout.layoffs.length > 0 ? (
        <p className="gr-seat-note">{`Laid off ${layout.layoffs.map((l) => cardLabel(l.card)).join(" ")}`}</p>
      ) : null}
    </section>
  );
}

/**
 * The end-of-hand panel: both hands laid out as melds and deadwood, the
 * lay-offs attached to the melds they joined, the verdict and the points, with
 * the running scoreboard underneath.
 */
export function HandResultPanel({ result, score, names, onNext, onTally, onMenu }: HandResultPanelProps) {
  const primary = useFocusOnMount<HTMLButtonElement>();
  const knocker = result.knocker;
  const defender: Seat | null = knocker === null ? null : knocker === 0 ? 1 : 0;
  const incoming = new Map<number, Card[]>();
  if (defender !== null) {
    for (const l of result.layouts[defender].layoffs) {
      incoming.set(l.meld, [...(incoming.get(l.meld) ?? []), l.card]);
    }
  }
  const winner = result.winner;
  const over = score.winner !== null;
  const verdict =
    winner === null
      ? "No score"
      : `${names[winner]} ${winner === 0 ? "score" : "scores"} ${result.points}`;

  return (
    <div className="gr-overlay" role="dialog" aria-modal aria-label="Hand result">
      <div className="gr-dialog is-wide">
        <h2 className="gr-dialog-title">{title(result, names)}</h2>
        <p className={cx("gr-verdict", winner === 0 && "is-good", winner === 1 && "is-bad")}>{verdict}</p>
        <p className="gr-dialog-note">{explain(result)}</p>

        <div className="gr-layouts">
          {([0, 1] as const).map((seat) => (
            <SeatTable
              key={seat}
              name={names[seat]}
              layout={result.layouts[seat]}
              knocked={knocker === seat}
              incoming={seat === knocker ? incoming : new Map()}
            />
          ))}
        </div>

        <p className="gr-standing">
          <span>{`${names[0]} ${score.scores[0]}`}</span>
          <span className="gr-standing-target">{over ? "game over" : `first to ${score.target}`}</span>
          <span>{`${names[1]} ${score.scores[1]}`}</span>
        </p>

        <details className="gr-details">
          <summary>Scoreboard</summary>
          <Scoreboard history={score.history} names={names} />
        </details>

        <div className="gr-dialog-actions">
          <button ref={primary} type="button" className="gr-primary" onClick={over ? onTally : onNext}>
            {over ? "Final tally" : "Next hand"}
          </button>
          <button type="button" className="gr-ghost" onClick={onMenu}>
            Menu
          </button>
        </div>
      </div>
    </div>
  );
}
