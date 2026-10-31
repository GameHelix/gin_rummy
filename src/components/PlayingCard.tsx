import { cx } from "@/lib/cx";
import { isRed, RANK_LABEL, rankOf, SUIT_GLYPH, suitOf } from "@/lib/game/cards";
import type { Card } from "@/lib/game/types";

interface CardFaceProps {
  card: Card;
  /** The small size used in result panels. */
  mini?: boolean;
  /** Meld colour index (0–3); omit or -1 for none. */
  meld?: number;
  /** Mark the card as unmelded deadwood. */
  dead?: boolean;
  selected?: boolean;
  /** Just drawn this turn. */
  fresh?: boolean;
  /** Laid off onto the knocker's meld at the end of a hand. */
  laidOff?: boolean;
}

/**
 * A neon playing card drawn entirely in CSS: rank and suit in two corners, a
 * big glowing centre pip, red suits in rose and black suits in cold white on
 * dark glass. Purely visual — the clickable wrapper supplies the label.
 */
export function CardFace({ card, mini, meld = -1, dead, selected, fresh, laidOff }: CardFaceProps) {
  const rank = RANK_LABEL[rankOf(card)];
  const suit = SUIT_GLYPH[suitOf(card)];
  return (
    <span
      className={cx(
        "gr-card",
        isRed(card) ? "is-red" : "is-black",
        rankOf(card) > 10 && "is-court",
        mini && "is-mini",
        meld >= 0 && `is-meld gr-m${meld % 4}`,
        dead && "is-dead",
        selected && "is-selected",
        fresh && "is-fresh",
        laidOff && "is-laidoff"
      )}
      aria-hidden
    >
      <span className="gr-card-corner">
        <span className="gr-card-rank">{rank}</span>
        <span className="gr-card-suit">{suit}</span>
      </span>
      <span className="gr-card-pip">{suit}</span>
      <span className="gr-card-corner is-end">
        <span className="gr-card-rank">{rank}</span>
        <span className="gr-card-suit">{suit}</span>
      </span>
    </span>
  );
}

/** The face-down neon back. */
export function CardBack({ mini }: { mini?: boolean }) {
  return <span className={cx("gr-card", "is-back", mini && "is-mini")} aria-hidden />;
}
