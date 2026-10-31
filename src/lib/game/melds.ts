import { cardValue, compareByRank, rankOf, suitOf } from "./cards";
import type { Arrangement, Card, Meld } from "./types";

/**
 * The optimal meld finder.
 *
 * Greedy melding is wrong in Gin: a card can belong to a set *or* a run, and a
 * four of a kind can be split three ways. So the search is exhaustive, made
 * cheap by working on bitmasks of hand positions:
 *
 *  1. list every candidate meld in the hand — each set of three, each four of a
 *     kind and its three-card subsets, and every run segment of length ≥ 3;
 *  2. solve `best(mask)` = the least deadwood the cards in `mask` can leave, by
 *     deciding the lowest card in the mask: count it as deadwood, or use one of
 *     the candidate melds whose lowest card it is. Memoised per mask, this
 *     visits each subset at most once (2^11 = 2048 for a full hand).
 *
 * Because every sub-mask is solved on the way, "deadwood after discarding card
 * i" for all eleven cards shares one memo table — the CPU leans on that.
 */

/** Three or four cards of one rank. */
export function isSet(cards: readonly Card[]): boolean {
  if (cards.length < 3 || cards.length > 4) return false;
  const r = rankOf(cards[0]);
  for (const c of cards) if (rankOf(c) !== r) return false;
  return new Set(cards).size === cards.length;
}

/** Three or more consecutive cards of one suit; the ace is low only (no K-A wrap). */
export function isRun(cards: readonly Card[]): boolean {
  if (cards.length < 3) return false;
  const s = suitOf(cards[0]);
  for (const c of cards) if (suitOf(c) !== s) return false;
  const ranks = cards.map(rankOf).sort((a, b) => a - b);
  for (let i = 1; i < ranks.length; i++) if (ranks[i] !== ranks[i - 1] + 1) return false;
  return true;
}

export function isMeld(cards: readonly Card[]): boolean {
  return isSet(cards) || isRun(cards);
}

/** Hands larger than this are never melded during play (11 is the maximum). */
const MAX_CARDS = 16;

interface Prepared {
  cards: readonly Card[];
  n: number;
  values: Int8Array;
  /** Candidate meld masks grouped by their lowest member's position. */
  byLow: number[][];
  meldCount: number;
  memo: Int16Array;
}

/** Scratch lookup from card id to hand position; always restored to -1 after use. */
const POSITION = new Int8Array(52).fill(-1);

function prepare(cards: readonly Card[]): Prepared {
  const n = cards.length;
  if (n > MAX_CARDS) throw new RangeError(`Cannot meld ${n} cards at once`);
  const values = new Int8Array(n);
  const byLow: number[][] = [];
  for (let i = 0; i < n; i++) {
    values[i] = cardValue(cards[i]);
    byLow.push([]);
  }
  let meldCount = 0;
  const add = (mask: number) => {
    byLow[31 - Math.clz32(mask & -mask)].push(mask);
    meldCount++;
  };

  for (let i = 0; i < n; i++) POSITION[cards[i]] = i;

  // Sets: every rank held three or four times.
  for (let rank = 0; rank < 13; rank++) {
    let mask = 0;
    let count = 0;
    for (let s = 0; s < 4; s++) {
      const p = POSITION[s * 13 + rank];
      if (p >= 0) {
        mask |= 1 << p;
        count++;
      }
    }
    if (count === 3) add(mask);
    else if (count === 4) {
      add(mask);
      // Each three-card subset, so the fourth card stays free for a run.
      for (let s = 0; s < 4; s++) add(mask & ~(1 << POSITION[s * 13 + rank]));
    }
  }

  // Runs: every segment of length ≥ 3 inside each maximal same-suit sequence.
  for (let s = 0; s < 4; s++) {
    const base = s * 13;
    let r = 0;
    while (r < 13) {
      if (POSITION[base + r] < 0) {
        r++;
        continue;
      }
      let end = r;
      while (end + 1 < 13 && POSITION[base + end + 1] >= 0) end++;
      if (end - r >= 2) {
        for (let a = r; a <= end - 2; a++) {
          let mask = 0;
          for (let b = a; b <= end; b++) {
            mask |= 1 << POSITION[base + b];
            if (b - a >= 2) add(mask);
          }
        }
      }
      r = end + 1;
    }
  }

  for (let i = 0; i < n; i++) POSITION[cards[i]] = -1;

  const memo = new Int16Array(meldCount > 0 ? 1 << n : 1);
  memo.fill(-1);
  return { cards, n, values, byLow, meldCount, memo };
}

function maskValue(p: Prepared, mask: number): number {
  let total = 0;
  for (let m = mask; m !== 0; m &= m - 1) total += p.values[31 - Math.clz32(m & -m)];
  return total;
}

/** Least deadwood the cards in `mask` can be left with. */
function solve(p: Prepared, mask: number): number {
  if (mask === 0) return 0;
  if (p.meldCount === 0) return maskValue(p, mask);
  const hit = p.memo[mask];
  if (hit >= 0) return hit;
  const low = mask & -mask;
  const i = 31 - Math.clz32(low);
  let best = p.values[i] + solve(p, mask ^ low);
  const list = p.byLow[i];
  for (let k = 0; k < list.length; k++) {
    const m = list[k];
    if ((m & mask) === m) {
      const v = solve(p, mask ^ m);
      if (v < best) best = v;
    }
  }
  p.memo[mask] = best;
  return best;
}

function fullMask(n: number): number {
  return n >= 31 ? -1 : (1 << n) - 1;
}

function maskCards(p: Prepared, mask: number): Card[] {
  const out: Card[] = [];
  for (let i = 0; i < p.n; i++) if (mask & (1 << i)) out.push(p.cards[i]);
  return out;
}

function toMeld(cards: Card[]): Meld {
  if (isSet(cards)) return { kind: "set", cards: [...cards].sort((a, b) => suitOf(a) - suitOf(b)) };
  return { kind: "run", cards: [...cards].sort((a, b) => a - b) };
}

/** Melds first by their lowest rank, then by suit, so layouts read naturally. */
function compareMelds(a: Meld, b: Meld): number {
  return compareByRank(a.cards[0], b.cards[0]);
}

function build(p: Prepared, meldMasks: number[], deadMask: number): Arrangement {
  const melds = meldMasks.map((m) => toMeld(maskCards(p, m))).sort(compareMelds);
  const deadwood = maskCards(p, deadMask).sort(compareByRank);
  return { melds, deadwood, deadwoodValue: maskValue(p, deadMask) };
}

/** Walk the memo table back down to one optimal arrangement of `mask`. */
function reconstruct(p: Prepared, mask: number): Arrangement {
  const melds: number[] = [];
  let dead = 0;
  let rest = mask;
  while (rest !== 0) {
    const target = solve(p, rest);
    const low = rest & -rest;
    const i = 31 - Math.clz32(low);
    let used = 0;
    // Prefer a meld on ties so a hand is shown with as much melded as possible.
    for (const m of p.byLow[i] ?? []) {
      if ((m & rest) === m && solve(p, rest ^ m) === target) {
        used = m;
        break;
      }
    }
    if (used) {
      melds.push(used);
      rest ^= used;
    } else {
      dead |= low;
      rest ^= low;
    }
  }
  return build(p, melds, dead);
}

/** Least possible deadwood for `cards`. */
export function deadwoodOf(cards: readonly Card[]): number {
  const p = prepare(cards);
  return solve(p, fullMask(p.n));
}

/** One optimal (minimum-deadwood) arrangement of `cards`. */
export function bestArrangement(cards: readonly Card[]): Arrangement {
  const p = prepare(cards);
  return reconstruct(p, fullMask(p.n));
}

/**
 * Every minimum-deadwood arrangement of `cards` (up to `limit`). Different
 * arrangements can expose different lay-off spots, so the knocker picks among
 * them — see `scoring.ts`.
 */
export function allBestArrangements(cards: readonly Card[], limit = 48): Arrangement[] {
  const p = prepare(cards);
  const out: Arrangement[] = [];
  const melds: number[] = [];
  const walk = (rest: number, dead: number) => {
    if (out.length >= limit) return;
    if (rest === 0) {
      out.push(build(p, melds, dead));
      return;
    }
    const target = solve(p, rest);
    const low = rest & -rest;
    const i = 31 - Math.clz32(low);
    for (const m of p.byLow[i] ?? []) {
      if ((m & rest) === m && solve(p, rest ^ m) === target) {
        melds.push(m);
        walk(rest ^ m, dead);
        melds.pop();
      }
    }
    if (p.values[i] + solve(p, rest ^ low) === target) walk(rest ^ low, dead | low);
  };
  walk(fullMask(p.n), 0);
  return out;
}

/**
 * Deadwood left after discarding each card in turn: `result[i]` is the least
 * deadwood of `cards` without `cards[i]`. One shared memo table serves all of
 * them.
 */
export function deadwoodAfterEachDiscard(cards: readonly Card[]): number[] {
  const p = prepare(cards);
  const full = fullMask(p.n);
  const out: number[] = [];
  for (let i = 0; i < p.n; i++) out.push(solve(p, full ^ (1 << i)));
  return out;
}

/**
 * The least deadwood reachable by discarding one card from `cards` (usually
 * eleven), never discarding `forbid` — the card just taken from the pile.
 */
export function bestDeadwoodAfterDiscard(cards: readonly Card[], forbid: Card | null = null): number {
  const p = prepare(cards);
  const full = fullMask(p.n);
  let best = Infinity;
  for (let i = 0; i < p.n; i++) {
    if (cards[i] === forbid) continue;
    const v = solve(p, full ^ (1 << i));
    if (v < best) best = v;
  }
  return best;
}

/** Is every card of `cards` covered by melds? */
export function isFullyMelded(cards: readonly Card[]): boolean {
  return cards.length > 0 && deadwoodOf(cards) === 0;
}

/** The candidate melds a hand contains (for tests and hints), as card lists. */
export function candidateMelds(cards: readonly Card[]): Card[][] {
  const p = prepare(cards);
  const out: Card[][] = [];
  for (const list of p.byLow) for (const m of list) out.push(maskCards(p, m));
  return out;
}
