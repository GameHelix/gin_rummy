"use client";

import { cx } from "@/lib/cx";
import { useFocusOnMount } from "@/lib/hooks/useFocusOnMount";
import { BOX_BONUS, DIFFICULTY_LABEL } from "@/lib/game/constants";
import type { FinalTally, HandRecord } from "@/lib/game/match";
import type { Difficulty } from "@/lib/game/types";
import { Scoreboard } from "./Scoreboard";

export interface TierRecord {
  won: number;
  lost: number;
  /** Largest winning margin, 0 until the first win. */
  best: number;
}

interface ResultPanelProps {
  tally: FinalTally;
  history: readonly HandRecord[];
  names: readonly [string, string];
  difficulty: Difficulty;
  record: TierRecord;
  isNewBest: boolean;
  onPlayAgain: () => void;
  onMenu: () => void;
}

/** The game's final tally: points, the game bonus, boxes and any shutout, then the margin. */
export function ResultPanel({
  tally,
  history,
  names,
  difficulty,
  record,
  isNewBest,
  onPlayAgain,
  onMenu,
}: ResultPanelProps) {
  const again = useFocusOnMount<HTMLButtonElement>();
  const youWon = tally.winner === 0;
  const row = (label: string, values: readonly [string, string]) => (
    <tr>
      <th scope="row">{label}</th>
      <td>{values[0]}</td>
      <td>{values[1]}</td>
    </tr>
  );
  const boxes = (seat: 0 | 1) =>
    tally.handsWon[seat] > 0 ? `${tally.handsWon[seat]} × ${BOX_BONUS}` : "0";

  return (
    <div className="gr-overlay" role="dialog" aria-modal aria-label="Game result">
      <div className="gr-dialog">
        <h2 className="gr-dialog-title">{youWon ? "🏆 You win the game!" : "💀 The CPU wins"}</h2>
        <p className={cx("gr-margin", youWon ? "is-good" : "is-bad")}>{`${youWon ? "+" : "−"}${tally.margin}`}</p>
        <p className="gr-dialog-note">
          {youWon ? "Your winning margin" : "The CPU's winning margin"}
          {tally.shutout ? " — a shutout, so everything doubled" : ""}
        </p>

        <table className="gr-tally">
          <thead>
            <tr>
              <th scope="col" />
              <th scope="col">{names[0]}</th>
              <th scope="col">{names[1]}</th>
            </tr>
          </thead>
          <tbody>
            {row("Hand points", [String(tally.points[0]), String(tally.points[1])])}
            {row("Game bonus", [String(tally.gameBonus[0]), String(tally.gameBonus[1])])}
            {row("Boxes", [boxes(0), boxes(1)])}
            {tally.shutout ? row("Shutout", [youWon ? "× 2" : "", youWon ? "" : "× 2"]) : null}
            <tr className="is-total">
              <th scope="row">Total</th>
              <td>{tally.totals[0]}</td>
              <td>{tally.totals[1]}</td>
            </tr>
          </tbody>
        </table>

        <p className="gr-record">
          {`${DIFFICULTY_LABEL[difficulty]}: ${record.won} won · ${record.lost} lost`}
          {isNewBest ? (
            <b className="gr-newbest">New best margin!</b>
          ) : record.best > 0 ? (
            <span>{` · best margin ${record.best}`}</span>
          ) : null}
        </p>

        <details className="gr-details">
          <summary>Hand by hand</summary>
          <Scoreboard history={history} names={names} />
        </details>

        <button ref={again} type="button" className="gr-primary" onClick={onPlayAgain}>
          Play again
        </button>
        <button type="button" className="gr-ghost" onClick={onMenu}>
          Back to menu
        </button>
      </div>
    </div>
  );
}
