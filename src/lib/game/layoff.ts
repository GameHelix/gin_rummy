import { makeCard, rankOf, suitOf } from "./cards";
import { bestArrangement, deadwoodOf } from "./melds";
import type { Arrangement, Card, Layoff, Meld } from "./types";

/**
 * Lay-offs: when a player knocks, the defender may add cards to the knocker's
 * melds — the missing fourth card of a set, or cards extending a run at either
 * end. Lay-offs chain: with 5♠6♠7♠ on the table, 8♠ and then 9♠ both go on.
 *
 * The defender's choice is a real optimisation. A card that could be laid off
 * might do more good inside one of the defender's own melds, so the best plan
 * is found by trying every subset of the possible lay-off cards (rarely more
 * than a handful), keeping those that can actually be attached, and melding
 * whatever remains optimally.
 */

export interface LayoffPlan {
  layoffs: Layoff[];
  /** The defender's remaining cards, melded optimally. */
  arrangement: Arrangement;
}

function runBounds(meld: Meld): { suit: number; lo: number; hi: number } {
  return {
    suit: suitOf(meld.cards[0]),
    lo: rankOf(meld.cards[0]),
    hi: rankOf(meld.cards[meld.cards.length - 1]),
  };
}

/** The suit missing from a three-card set, or -1 for a full four. */
function missingSuit(meld: Meld): number {
  if (meld.cards.length !== 3) return -1;
  const present = new Set(meld.cards.map(suitOf));
  for (let s = 0; s < 4; s++) if (!present.has(s as 0 | 1 | 2 | 3)) return s;
  return -1;
}

/**
 * Every defender card that could possibly be laid off on `melds`, following
 * chains through the defender's own cards.
 */
export function layoffCandidates(melds: readonly Meld[], defender: readonly Card[]): Card[] {
  const have = new Set(defender);
  const out = new Set<Card>();
  for (const meld of melds) {
    if (meld.kind === "run") {
      const { suit, lo, hi } = runBounds(meld);
      for (let r = lo - 1; r >= 1 && have.has(makeCard(suit, r)); r--) out.add(makeCard(suit, r));
      for (let r = hi + 1; r <= 13 && have.has(makeCard(suit, r)); r++) out.add(makeCard(suit, r));
    } else {
      const s = missingSuit(meld);
      if (s >= 0) {
        const c = makeCard(s, rankOf(meld.cards[0]));
        if (have.has(c)) out.add(c);
      }
    }
  }
  return [...out].sort((a, b) => a - b);
}

/**
 * Attach exactly the cards in `cards` to `melds`, or return `null` if some
 * card cannot be attached. Runs are extended first, walking outward through
 * consecutive cards (that is the chaining); a set then takes its missing
 * fourth card. Greedy is safe here: a set has one possible extra card, so a
 * run that claims it never blocks anything else.
 */
export function assignLayoffs(melds: readonly Meld[], cards: readonly Card[]): Layoff[] | null {
  const pending = new Set(cards);
  const out: Layoff[] = [];
  melds.forEach((meld, index) => {
    if (meld.kind !== "run") return;
    const { suit, lo, hi } = runBounds(meld);
    for (let r = lo - 1; r >= 1 && pending.has(makeCard(suit, r)); r--) {
      pending.delete(makeCard(suit, r));
      out.push({ card: makeCard(suit, r), meld: index });
    }
    for (let r = hi + 1; r <= 13 && pending.has(makeCard(suit, r)); r++) {
      pending.delete(makeCard(suit, r));
      out.push({ card: makeCard(suit, r), meld: index });
    }
  });
  melds.forEach((meld, index) => {
    if (meld.kind !== "set") return;
    const s = missingSuit(meld);
    if (s < 0) return;
    const c = makeCard(s, rankOf(meld.cards[0]));
    if (pending.has(c)) {
      pending.delete(c);
      out.push({ card: c, meld: index });
    }
  });
  return pending.size === 0 ? out : null;
}

/**
 * The defender's best response to a knock: which cards to lay off and how to
 * meld the rest, minimising the deadwood that counts against them. On ties the
 * plan with fewer lay-offs wins, which keeps the defender's own melds intact.
 */
export function bestLayoffPlan(melds: readonly Meld[], defender: readonly Card[]): LayoffPlan {
  const candidates = layoffCandidates(melds, defender);
  const k = candidates.length;
  if (k === 0) return { layoffs: [], arrangement: bestArrangement(defender) };

  let bestDead = deadwoodOf(defender);
  let bestLayoffs: Layoff[] = [];
  let bestGone: Set<Card> = new Set();
  for (let mask = 1; mask < 1 << k; mask++) {
    const chosen: Card[] = [];
    for (let i = 0; i < k; i++) if (mask & (1 << i)) chosen.push(candidates[i]);
    const layoffs = assignLayoffs(melds, chosen);
    if (!layoffs) continue;
    const gone = new Set(chosen);
    const dead = deadwoodOf(defender.filter((c) => !gone.has(c)));
    if (dead < bestDead || (dead === bestDead && layoffs.length < bestLayoffs.length)) {
      bestDead = dead;
      bestLayoffs = layoffs;
      bestGone = gone;
    }
  }
  return {
    layoffs: bestLayoffs,
    arrangement: bestArrangement(defender.filter((c) => !bestGone.has(c))),
  };
}

/** Just the deadwood a defender is left with after the best lay-offs — the CPU's fast path. */
export function deadwoodAfterLayoffs(melds: readonly Meld[], defender: readonly Card[]): number {
  const candidates = layoffCandidates(melds, defender);
  const k = candidates.length;
  let best = deadwoodOf(defender);
  for (let mask = 1; mask < 1 << k; mask++) {
    const chosen: Card[] = [];
    for (let i = 0; i < k; i++) if (mask & (1 << i)) chosen.push(candidates[i]);
    if (!assignLayoffs(melds, chosen)) continue;
    const gone = new Set(chosen);
    const dead = deadwoodOf(defender.filter((c) => !gone.has(c)));
    if (dead < best) best = dead;
  }
  return best;
}
