import { cardLabel, cardValue, sortByRank, sortBySuit } from "./cards";
import { KNOCK_LIMIT } from "./constants";
import { bestArrangement, deadwoodAfterEachDiscard, deadwoodOf } from "./melds";
import type { Arrangement, Card, HandState, PublicEvent, Seat } from "./types";

/**
 * Presentation helpers that are still pure: how to order a hand on screen and
 * how to put the last move into words. Kept out of the components so they can
 * be tested and reused without a DOM.
 */

/** How the player's hand is laid out: grouped by melds, sorted, or hand-arranged. */
export type SortMode = "melds" | "rank" | "suit" | "custom";

export interface DisplayCard {
  card: Card;
  /** Index of the meld the card belongs to in the best arrangement, or -1 for deadwood. */
  meld: number;
}

export interface DisplayGroup {
  key: string;
  cards: DisplayCard[];
}

export interface HandLayout {
  groups: DisplayGroup[];
  /** The cards left to right, flattened. */
  order: Card[];
  arrangement: Arrangement;
}

/**
 * Lay a hand out for display. In "melds" mode each meld is its own group and
 * the deadwood trails behind; the other modes keep one row but still tag every
 * card with its meld so it can be coloured.
 */
export function layoutHand(hand: readonly Card[], mode: SortMode, custom: readonly Card[]): HandLayout {
  const arrangement = bestArrangement(hand);
  const meldOf = new Map<Card, number>();
  arrangement.melds.forEach((m, i) => m.cards.forEach((c) => meldOf.set(c, i)));
  const tag = (card: Card): DisplayCard => ({ card, meld: meldOf.get(card) ?? -1 });

  let groups: DisplayGroup[];
  if (mode === "melds") {
    groups = arrangement.melds.map((m, i) => ({ key: `meld-${i}`, cards: m.cards.map(tag) }));
    if (arrangement.deadwood.length > 0) {
      groups.push({ key: "deadwood", cards: arrangement.deadwood.map(tag) });
    }
  } else {
    let row: Card[];
    if (mode === "rank") row = sortByRank(hand);
    else if (mode === "suit") row = sortBySuit(hand);
    else {
      const held = new Set(hand);
      const kept = custom.filter((c) => held.has(c));
      const placed = new Set(kept);
      row = [...kept, ...hand.filter((c) => !placed.has(c))];
    }
    groups = [{ key: "row", cards: row.map(tag) }];
  }
  return { groups, order: groups.flatMap((g) => g.cards.map((c) => c.card)), arrangement };
}

export interface DiscardChoices {
  /** Deadwood kept after each legal discard. */
  after: Map<Card, number>;
  /** The legal discard leaving the least deadwood, when that is a knock (1–10). */
  knock: { card: Card; deadwood: number } | null;
  /** A legal discard that leaves nothing unmelded. */
  gin: Card | null;
  /** All eleven cards meld as they are. */
  bigGin: boolean;
  /** Least deadwood any legal discard can leave. */
  best: number;
}

/**
 * What the eleven-card hand can do right now, for the Knock and Gin buttons
 * and the live counter. Ties shed the higher card.
 */
export function discardChoices(hand: readonly Card[], forbid: Card | null): DiscardChoices {
  const each = deadwoodAfterEachDiscard(hand);
  const after = new Map<Card, number>();
  let pick: Card | null = null;
  let best = Infinity;
  for (let i = 0; i < hand.length; i++) {
    const card = hand[i];
    if (card === forbid) continue;
    after.set(card, each[i]);
    const shedsMore = pick !== null && each[i] === best && cardValue(card) > cardValue(pick);
    if (each[i] < best || shedsMore) {
      best = each[i];
      pick = card;
    }
  }
  return {
    after,
    knock: pick !== null && best > 0 && best <= KNOCK_LIMIT ? { card: pick, deadwood: best } : null,
    gin: pick !== null && best === 0 ? pick : null,
    bigGin: deadwoodOf(hand) === 0,
    best,
  };
}

/** Move `card` to where `target` sits in `order` (drag-to-reorder). */
export function moveCard(order: readonly Card[], card: Card, target: Card): Card[] {
  if (card === target) return [...order];
  const without = order.filter((c) => c !== card);
  const at = without.indexOf(target);
  const from = order.indexOf(card);
  const to = order.indexOf(target);
  // Dropping onto a card further right lands after it; further left lands before it.
  without.splice(from < to ? at + 1 : at, 0, card);
  return without;
}

/** "the 7♥" — the article reads better in prompts. */
export function theCard(card: Card): string {
  return `the ${cardLabel(card)}`;
}

/** One line about a public event, from a given seat's point of view. */
export function describeEvent(e: PublicEvent, names: readonly [string, string]): string {
  const who = names[e.seat];
  switch (e.kind) {
    case "pass":
      return `${who} passed on ${theCard(e.card)}`;
    case "take":
      return `${who} took ${theCard(e.card)}`;
    case "draw":
      return `${who} drew from the stock`;
    case "discard":
      return `${who} discarded ${theCard(e.card)}`;
  }
}

/** The most recent thing `seat` did this hand, in words — or null. */
export function lastMoveBy(state: HandState, seat: Seat, names: readonly [string, string]): string | null {
  for (let i = state.log.length - 1; i >= 0; i--) {
    const e = state.log[i];
    if (e.seat === seat) {
      // A draw followed by a discard reads best as one sentence.
      if (e.kind === "discard" && i > 0 && state.log[i - 1].seat === seat) {
        const before = state.log[i - 1];
        if (before.kind === "draw") return `${names[seat]} drew and discarded ${theCard(e.card)}`;
        if (before.kind === "take") {
          return `${names[seat]} took ${theCard(before.card)} and discarded ${theCard(e.card)}`;
        }
      }
      return describeEvent(e, names);
    }
  }
  return null;
}
