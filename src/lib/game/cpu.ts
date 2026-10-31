import { cardValue, rankOf, suitOf } from "./cards";
import { GAME_TARGET, KNOCK_LIMIT } from "./constants";
import { feedRisk, readTable, type Knowledge } from "./knowledge";
import { deadwoodAfterLayoffs } from "./layoff";
import {
  bestArrangement,
  bestDeadwoodAfterDiscard,
  deadwoodAfterEachDiscard,
  deadwoodOf,
} from "./melds";
import type { Rng } from "./rng";
import { knockVerdict } from "./scoring";
import type { Action, Card, Difficulty, MatchContext, Meld, PlayerView } from "./types";

/**
 * The computer opponent. It decides from a `PlayerView` — its own cards plus
 * the public record — so it cannot see the human's hand or the stock order.
 *
 * - **Easy** mostly draws blind, sheds high deadwood with some randomness and
 *   knocks late.
 * - **Medium** takes a discard only when it melds or clearly lowers deadwood,
 *   discards to minimise deadwood (keeping connected cards on ties) and knocks
 *   when it is legal and still safe.
 * - **Hard** counts every card it has seen to know which draws are still live,
 *   reads the opponent's pickups and discards to avoid feeding their melds,
 *   simulates the opponent's likely hand to weigh the undercut before
 *   knocking, and holds out for gin when it is ahead.
 */

const NEUTRAL: MatchContext = { myScore: 0, oppScore: 0, target: GAME_TARGET };

export function chooseAction(
  view: PlayerView,
  difficulty: Difficulty,
  rng: Rng,
  ctx: MatchContext = NEUTRAL
): Action {
  if (view.turn !== view.seat || view.phase === "over") {
    throw new Error("The CPU was asked to move out of turn");
  }
  const k = readTable(view);
  switch (view.phase) {
    case "offer":
      return wantsTopDiscard(view, k, difficulty, rng) ? { type: "draw-discard" } : { type: "pass" };
    case "draw":
      if (view.stockOnly || k.top === null) return { type: "draw-stock" };
      return wantsTopDiscard(view, k, difficulty, rng)
        ? { type: "draw-discard" }
        : { type: "draw-stock" };
    case "discard":
      return chooseDiscard(view, k, difficulty, rng, ctx);
  }
}

/* --------------------------------------------------------------- drawing */

/** Does `card` slot into a meld of `hand` (rather than just sit as deadwood)? */
export function joinsMeld(hand: readonly Card[], card: Card): boolean {
  return deadwoodOf([...hand, card]) < deadwoodOf(hand) + cardValue(card);
}

/** Average best deadwood after a blind draw, over every card still unseen. */
function expectedAfterStockDraw(hand: readonly Card[], unseen: readonly Card[]): number {
  if (unseen.length === 0) return deadwoodOf(hand);
  let total = 0;
  for (const u of unseen) total += bestDeadwoodAfterDiscard([...hand, u]);
  return total / unseen.length;
}

function wantsTopDiscard(view: PlayerView, k: Knowledge, difficulty: Difficulty, rng: Rng): boolean {
  const top = k.top;
  if (top === null) return false;
  const hand = view.hand;
  const melds = joinsMeld(hand, top);
  const now = deadwoodOf(hand);
  const after = bestDeadwoodAfterDiscard([...hand, top], top);

  if (difficulty === "easy") {
    // Mostly blind draws: even a card that melds is taken only some of the time.
    return melds && rng.next() < 0.55;
  }
  if (difficulty === "medium") {
    return (melds && after <= now) || after <= now - 2;
  }
  // Hard: a sure improvement against the average blind draw. Taking also tells
  // the opponent what we collect, so a non-melding pickup must clearly win.
  if (melds && after < now) return true;
  const blind = expectedAfterStockDraw(hand, k.unseen);
  return after + 2 < blind;
}

/* ------------------------------------------------------------ discarding */

/** How many other cards in `hand` could meld with `card` — a cheap "potential" measure. */
export function connectivity(card: Card, hand: readonly Card[]): number {
  let score = 0;
  for (const c of hand) {
    if (c === card) continue;
    if (rankOf(c) === rankOf(card)) score += 1;
    else if (suitOf(c) === suitOf(card)) {
      const d = Math.abs(rankOf(c) - rankOf(card));
      if (d === 1) score += 1;
      else if (d === 2) score += 0.5;
    }
  }
  return score;
}

interface Option {
  index: number;
  card: Card;
  /** Deadwood of the ten cards kept. */
  after: number;
}

function legalOptions(view: PlayerView): Option[] {
  const after = deadwoodAfterEachDiscard(view.hand);
  const out: Option[] = [];
  view.hand.forEach((card, index) => {
    if (card !== view.takenFromDiscard) out.push({ index, card, after: after[index] });
  });
  return out;
}

function without(hand: readonly Card[], index: number): Card[] {
  return hand.filter((_, i) => i !== index);
}

/** Least deadwood first; ties keep the better-connected card, then shed the higher one. */
function compareMedium(hand: readonly Card[]) {
  return (a: Option, b: Option) =>
    a.after - b.after ||
    connectivity(a.card, hand) - connectivity(b.card, hand) ||
    cardValue(b.card) - cardValue(a.card);
}

function chooseDiscard(
  view: PlayerView,
  k: Knowledge,
  difficulty: Difficulty,
  rng: Rng,
  ctx: MatchContext
): Action {
  const hand = view.hand;
  if (deadwoodOf(hand) === 0) return { type: "big-gin" };

  const options = legalOptions(view);
  // Every tier goes gin when it can: shed the highest card that still leaves zero.
  const gin = options
    .filter((o) => o.after === 0)
    .sort((a, b) => cardValue(b.card) - cardValue(a.card))[0];
  if (gin) return { type: "knock", card: gin.card };

  const lowest = [...options].sort(compareMedium(hand))[0];

  if (difficulty === "easy") {
    const knockLate = lowest.after <= 3 || (lowest.after <= KNOCK_LIMIT && k.stockCount <= 10);
    if (knockLate) return { type: "knock", card: lowest.card };
    return { type: "discard", card: easyDiscard(hand, options, rng).card };
  }

  if (difficulty === "medium") {
    if (lowest.after <= KNOCK_LIMIT && mediumKnockIsSafe(lowest.after, k)) {
      return { type: "knock", card: lowest.card };
    }
    return { type: "discard", card: lowest.card };
  }

  if (lowest.after <= KNOCK_LIMIT && hardShouldKnock(hand, lowest, k, rng, ctx)) {
    return { type: "knock", card: lowest.card };
  }
  return { type: "discard", card: hardDiscard(hand, options, k).card };
}

/**
 * Easy sheds unmelded cards, favouring high ones but not reliably, and only
 * breaks a meld when there is nothing else to throw.
 */
function easyDiscard(hand: readonly Card[], options: Option[], rng: Rng): Option {
  const dead = new Set(bestArrangement(hand).deadwood);
  const loose = options.filter((o) => dead.has(o.card));
  if (loose.length === 0) return [...options].sort((a, b) => a.after - b.after)[0];
  if (rng.next() < 0.25) return rng.pick(loose);
  return loose[rng.weighted(loose.map((o) => cardValue(o.card) ** 2))];
}

/**
 * Medium's sense of safety: an early knock rarely gets undercut, a late one
 * needs a low count — unless the stock is nearly gone and a draw looms.
 */
function mediumKnockIsSafe(deadwood: number, k: Knowledge): boolean {
  if (k.stockCount <= 6) return true;
  const oppTurns = k.oppStockDraws + k.oppPickups;
  if (oppTurns <= 4) return true;
  if (oppTurns <= 8) return deadwood <= 7;
  return deadwood <= 4;
}

/* --------------------------------------------------------- hard thinking */

/** Unseen cards that would hand us gin on the very next draw. */
export function ginOuts(hand: readonly Card[], unseen: readonly Card[]): number {
  let outs = 0;
  for (const u of unseen) if (bestDeadwoodAfterDiscard([...hand, u]) === 0) outs++;
  return outs;
}

export interface UndercutEstimate {
  /** Share of simulated opponent hands that would undercut us. */
  risk: number;
  /** Their average deadwood after lay-offs. */
  meanDeadwood: number;
  /** Our average score from knocking: the deadwood gap, or minus the undercut payout. */
  value: number;
}

/** Blind draws the simulated opponent gets to improve with; more adds noise, not insight. */
const MAX_SIMULATED_DRAWS = 12;

/**
 * Estimate what knocking with `melds` and `deadwood` is worth by dealing the
 * opponent plausible hands: the cards we saw them pick up, the rest drawn from
 * unseen cards, then improved greedily once per blind draw they have made (a
 * fresh random hand would be far too weak by mid-game). Each simulated hand
 * lays off against our melds and is scored against ours, undercuts included.
 */
export function estimateUndercut(
  melds: readonly Meld[],
  deadwood: number,
  k: Knowledge,
  rng: Rng,
  samples = 24
): UndercutEstimate {
  const known = new Set(k.oppHas);
  const unknownCount = Math.max(0, k.oppHandSize - k.oppHas.length);
  const improvements = Math.min(k.oppStockDraws, MAX_SIMULATED_DRAWS);
  let under = 0;
  let total = 0;
  let value = 0;
  for (let s = 0; s < samples; s++) {
    const pool = rng.shuffle([...k.unseen]);
    const opp = [...k.oppHas, ...pool.slice(0, unknownCount)];
    let next = unknownCount;
    for (let step = 0; step < improvements && next < pool.length; step++) {
      opp.push(pool[next++]);
      const after = deadwoodAfterEachDiscard(opp);
      let drop = -1;
      for (let i = 0; i < opp.length; i++) {
        if (known.has(opp[i])) continue;
        if (drop < 0 || after[i] < after[drop]) drop = i;
      }
      opp.splice(drop < 0 ? opp.length - 1 : drop, 1);
    }
    const theirs = deadwoodAfterLayoffs(melds, opp);
    const verdict = knockVerdict(deadwood, theirs);
    if (verdict.outcome === "undercut") under++;
    total += theirs;
    value += verdict.winner === "knocker" ? verdict.points : -verdict.points;
  }
  return { risk: under / samples, meanDeadwood: total / samples, value: value / samples };
}

/**
 * Knock only when the simulated outcome is worth it: an expected gain of at
 * least this many points, so a likely undercut (which costs 25 extra) stops a
 * thin knock, while an early knock against an undeveloped hand goes ahead.
 */
const KNOCK_VALUE_FLOOR = 5;

function hardShouldKnock(
  hand: readonly Card[],
  knock: Option,
  k: Knowledge,
  rng: Rng,
  ctx: MatchContext
): boolean {
  const kept = without(hand, knock.index);
  const arrangement = bestArrangement(kept);

  // Ahead on the scoreboard with a hand one card from gin: play for the bonus
  // while the stock is still deep enough to give it a real chance.
  const ahead = ctx.myScore > ctx.oppScore;
  if (ahead && knock.after <= 4 && k.stockCount >= 10 && ginOuts(kept, k.unseen) >= 2) {
    const estimate = estimateUndercut(arrangement.melds, knock.after, k, rng, 12);
    if (estimate.risk < 0.25) return false;
  }

  return estimateUndercut(arrangement.melds, knock.after, k, rng).value >= KNOCK_VALUE_FLOOR;
}

/** Points of deadwood a fed meld is worth to the opponent, scaled by how likely it is. */
const FEED_COST = 7;

/**
 * Hard's discard: keep the hand with the lowest expected deadwood after the
 * next blind draw (counting only cards still live), plus how likely the
 * discard is to feed the opponent. As the hand wears on, a knock against us
 * grows likelier, so the deadwood we would be caught holding weighs more.
 */
function hardDiscard(hand: readonly Card[], options: Option[], k: Knowledge): Option {
  const exposure = 0.05 * (k.oppStockDraws + k.oppPickups);
  let best = options[0];
  let bestScore = Infinity;
  for (const o of options) {
    const kept = without(hand, o.index);
    const score =
      expectedAfterStockDraw(kept, k.unseen) +
      exposure * o.after +
      FEED_COST * feedRisk(o.card, k);
    if (score < bestScore - 1e-9) {
      best = o;
      bestScore = score;
    }
  }
  return best;
}
