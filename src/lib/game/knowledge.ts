import { fullDeck, rankOf, suitOf } from "./cards";
import { other } from "./round";
import type { Card, PlayerView, Seat } from "./types";

/**
 * What a player can honestly deduce at the table: their own cards, the
 * face-up pile, and the public record of who took, declined and discarded
 * what. Built from a `PlayerView` only, so the opponent's hidden cards and the
 * stock order are simply not available to the CPU.
 */
export interface Knowledge {
  seat: Seat;
  hand: readonly Card[];
  /** The top of the discard pile, if any. */
  top: Card | null;
  /** Cards the opponent picked up from the pile and has not thrown back. */
  oppHas: Card[];
  /** Cards the opponent discarded — a hint at what they are not collecting. */
  oppDiscarded: Card[];
  /** Upcards and discards the opponent passed over in favour of the stock. */
  oppDeclined: Card[];
  /** Cards that could still be in the stock or among the opponent's unknown cards. */
  unseen: Card[];
  oppHandSize: number;
  /** Blind stock draws by the opponent — each a chance for them to improve. */
  oppStockDraws: number;
  oppPickups: number;
  stockCount: number;
}

export function readTable(view: PlayerView): Knowledge {
  const opp = other(view.seat);
  const oppHas = new Set<Card>();
  const oppDiscarded = new Set<Card>();
  const oppDeclined = new Set<Card>();
  let oppStockDraws = 0;
  let oppPickups = 0;

  for (const e of view.log) {
    if (e.seat !== opp) continue;
    switch (e.kind) {
      case "take":
        oppHas.add(e.card);
        oppPickups++;
        break;
      case "discard":
        oppHas.delete(e.card);
        oppDiscarded.add(e.card);
        break;
      case "pass":
        oppDeclined.add(e.card);
        break;
      case "draw":
        oppStockDraws++;
        if (e.declined !== null) oppDeclined.add(e.declined);
        break;
    }
  }

  const known = new Set<Card>([...view.hand, ...view.discard, ...oppHas]);
  return {
    seat: view.seat,
    hand: view.hand,
    top: view.discard.length > 0 ? view.discard[view.discard.length - 1] : null,
    oppHas: [...oppHas],
    oppDiscarded: [...oppDiscarded],
    oppDeclined: [...oppDeclined].filter((c) => !oppHas.has(c)),
    unseen: fullDeck().filter((c) => !known.has(c)),
    oppHandSize: view.oppHandSize,
    oppStockDraws,
    oppPickups,
    stockCount: view.stockCount,
  };
}

/**
 * How likely the opponent is to want `card`, from 0 (it cannot help them) to 1.
 *
 * A card is only dangerous if a meld through it is still possible with cards
 * the opponent might hold. Within that, what they picked up raises the risk
 * for the same rank and nearby cards of the suit, while what they discarded or
 * declined lowers it.
 */
export function feedRisk(card: Card, k: Knowledge): number {
  const possible = new Set<Card>([...k.unseen, ...k.oppHas]);
  const rank = rankOf(card);
  const suit = suitOf(card);
  const at = (r: number) => (r >= 1 && r <= 13 ? possible.has(suit * 13 + r - 1) : false);

  let sameRank = 0;
  for (let s = 0; s < 4; s++) if (s !== suit && possible.has(s * 13 + rank - 1)) sameRank++;
  const canSet = sameRank >= 2;
  const canRun =
    (at(rank - 2) && at(rank - 1)) || (at(rank - 1) && at(rank + 1)) || (at(rank + 1) && at(rank + 2));
  if (!canSet && !canRun) return 0;

  let setRisk = 0;
  let runRisk = 0;
  if (canSet) {
    setRisk = 0.08;
    for (const c of k.oppHas) if (rankOf(c) === rank) setRisk += 0.45;
    for (const c of k.oppDiscarded) if (rankOf(c) === rank) setRisk -= 0.25;
    for (const c of k.oppDeclined) if (rankOf(c) === rank) setRisk -= 0.12;
  }
  if (canRun) {
    runRisk = 0.08;
    for (const c of k.oppHas) {
      if (suitOf(c) !== suit) continue;
      const d = Math.abs(rankOf(c) - rank);
      if (d === 1) runRisk += 0.4;
      else if (d === 2) runRisk += 0.25;
    }
    for (const c of [...k.oppDiscarded, ...k.oppDeclined]) {
      if (suitOf(c) !== suit) continue;
      const d = Math.abs(rankOf(c) - rank);
      if (d === 1) runRisk -= 0.15;
      else if (d === 2) runRisk -= 0.06;
    }
  }
  const clamp = (x: number) => Math.min(0.9, Math.max(0, x));
  return Math.min(1, clamp(setRisk) + clamp(runRisk));
}
