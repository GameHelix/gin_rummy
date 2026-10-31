"use client";

import { cx } from "@/lib/cx";
import { useFocusOnMount } from "@/lib/hooks/useFocusOnMount";
import type { HandRecord } from "@/lib/game/match";
import type { Outcome } from "@/lib/game/types";

export const OUTCOME_LABEL: Record<Outcome, string> = {
  knock: "Knock",
  gin: "Gin",
  "big-gin": "Big gin",
  undercut: "Undercut",
  draw: "Draw",
};

interface ScoreboardProps {
  history: readonly HandRecord[];
  names: readonly [string, string];
}

/** Hand-by-hand history: what ended each hand, who scored, and the running totals. */
export function Scoreboard({ history, names }: ScoreboardProps) {
  if (history.length === 0) return <p className="gr-empty">No hands finished yet.</p>;
  return (
    <div className="gr-scroll">
      <table className="gr-scoreboard">
        <thead>
          <tr>
            <th scope="col">#</th>
            <th scope="col">Result</th>
            <th scope="col">{names[0]}</th>
            <th scope="col">{names[1]}</th>
          </tr>
        </thead>
        <tbody>
          {history.map((h) => (
            <tr key={h.hand}>
              <td>{h.hand}</td>
              <td>{OUTCOME_LABEL[h.outcome]}</td>
              {([0, 1] as const).map((seat) => (
                <td key={seat} className={cx(h.winner === seat && "is-won")}>
                  {h.winner === seat ? <span className="gr-gain">{`+${h.points}`}</span> : null}
                  <span className="gr-total">{h.totals[seat]}</span>
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

interface ScoresDialogProps extends ScoreboardProps {
  scores: readonly [number, number];
  target: number;
  onClose: () => void;
}

export function ScoresDialog({ history, names, scores, target, onClose }: ScoresDialogProps) {
  const back = useFocusOnMount<HTMLButtonElement>();
  return (
    <div
      className="gr-overlay"
      role="dialog"
      aria-modal
      aria-label="Scoreboard"
      onKeyDown={(e) => {
        if (e.key === "Escape") onClose();
      }}
    >
      <div className="gr-dialog">
        <h2 className="gr-dialog-title">Scoreboard</h2>
        <p className="gr-dialog-note">
          {`${names[0]} ${scores[0]} · ${names[1]} ${scores[1]} — first to ${target}`}
        </p>
        <Scoreboard history={history} names={names} />
        <button ref={back} type="button" className="gr-primary" onClick={onClose}>
          Back to the table
        </button>
      </div>
    </div>
  );
}
