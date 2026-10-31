"use client";

import type { CSSProperties, DragEvent } from "react";
import { cx } from "@/lib/cx";
import { cardName } from "@/lib/game/cards";
import type { DisplayGroup } from "@/lib/game/display";
import { cardKey } from "@/lib/game/session";
import type { Card } from "@/lib/game/types";
import { CardFace } from "./PlayingCard";

/** The key that selects each position, left to right. */
export const SELECT_KEYS = ["1", "2", "3", "4", "5", "6", "7", "8", "9", "0", "-"] as const;

interface PlayerHandProps {
  groups: DisplayGroup[];
  selected: Card | null;
  fresh: Card | null;
  /** The card taken from the pile this turn — it cannot go straight back. */
  locked: Card | null;
  draggable: boolean;
  onSelect: (card: Card) => void;
  onDragStart: (card: Card) => void;
  onDragEnd: () => void;
  onDropOnCard: (target: Card) => void;
}

/**
 * The player's hand as an overlapping row. Melds and deadwood are coloured
 * (and, in meld order, spaced into groups); a tap selects, a drag drops on the
 * discard pile or onto another card to reorder.
 */
export function PlayerHand({
  groups,
  selected,
  fresh,
  locked,
  draggable,
  onSelect,
  onDragStart,
  onDragEnd,
  onDropOnCard,
}: PlayerHandProps) {
  const flat = groups.flatMap((g, gi) =>
    g.cards.map((dc, ci) => ({ ...dc, groupStart: gi > 0 && ci === 0 }))
  );
  const breaks = Math.max(0, groups.length - 1);

  return (
    <div className="gr-hand-wrap">
      <div
        className="gr-hand"
        role="group"
        aria-label="Your hand"
        style={{ "--n": flat.length, "--breaks": breaks } as CSSProperties}
      >
        {flat.map(({ card, meld, groupStart }, i) => {
          const label = `${cardName(card)}, ${meld >= 0 ? "melded" : "deadwood"}${
            locked === card ? ", just taken" : ""
          }`;
          return (
            <div
              key={card}
              className={cx("gr-slot", i > 0 && "is-stacked", groupStart && "is-group-start")}
              data-flip={cardKey(card)}
              draggable={draggable}
              onDragStart={(e: DragEvent<HTMLDivElement>) => {
                e.dataTransfer.setData("text/plain", String(card));
                e.dataTransfer.effectAllowed = "move";
                onDragStart(card);
              }}
              onDragEnd={onDragEnd}
              onDragOver={(e) => {
                if (draggable) e.preventDefault();
              }}
              onDrop={(e) => {
                e.preventDefault();
                onDropOnCard(card);
              }}
            >
              <button
                type="button"
                className="gr-card-btn"
                aria-label={label}
                aria-pressed={selected === card}
                onClick={() => onSelect(card)}
              >
                <CardFace
                  card={card}
                  meld={meld}
                  dead={meld < 0}
                  selected={selected === card}
                  fresh={fresh === card}
                />
              </button>
              <span className="gr-key-hint" aria-hidden>
                {SELECT_KEYS[i] ?? ""}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
