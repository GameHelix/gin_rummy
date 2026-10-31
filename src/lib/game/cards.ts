import type { Rng } from "./rng";
import type { Card, Suit } from "./types";

/**
 * Deck helpers. Cards are integers `suit * 13 + (rank - 1)`; everything here
 * converts between that compact form and what people read.
 */

export const SUITS: readonly Suit[] = [0, 1, 2, 3];

/**
 * Suit glyphs, each followed by U+FE0E (text presentation) so phones draw the
 * plain neon glyph instead of swapping in a colour emoji heart or diamond.
 */
export const SUIT_GLYPH: readonly string[] = ["♠︎", "♥︎", "♦︎", "♣︎"];

export const SUIT_NAME: readonly string[] = ["spades", "hearts", "diamonds", "clubs"];

/** Index by rank 1..13; index 0 is unused. */
export const RANK_LABEL: readonly string[] = [
  "",
  "A",
  "2",
  "3",
  "4",
  "5",
  "6",
  "7",
  "8",
  "9",
  "10",
  "J",
  "Q",
  "K",
];

const RANK_NAME: readonly string[] = [
  "",
  "ace",
  "two",
  "three",
  "four",
  "five",
  "six",
  "seven",
  "eight",
  "nine",
  "ten",
  "jack",
  "queen",
  "king",
];

export function makeCard(suit: number, rank: number): Card {
  return suit * 13 + (rank - 1);
}

export function suitOf(card: Card): Suit {
  return Math.floor(card / 13) as Suit;
}

/** 1 (ace) … 13 (king). */
export function rankOf(card: Card): number {
  return (card % 13) + 1;
}

/** Deadwood value: A = 1, 2–10 at face value, J/Q/K = 10. */
export function cardValue(card: Card): number {
  const r = (card % 13) + 1;
  return r > 10 ? 10 : r;
}

export function isRed(card: Card): boolean {
  const s = suitOf(card);
  return s === 1 || s === 2;
}

/** Short label such as `10♥` or `Q♠`. */
export function cardLabel(card: Card): string {
  return `${RANK_LABEL[rankOf(card)]}${SUIT_GLYPH[suitOf(card)]}`;
}

/** Spoken label such as "queen of spades", for screen readers. */
export function cardName(card: Card): string {
  return `${RANK_NAME[rankOf(card)]} of ${SUIT_NAME[suitOf(card)]}`;
}

/** Sum of card values, ignoring melds. */
export function pipTotal(cards: readonly Card[]): number {
  let total = 0;
  for (const c of cards) total += cardValue(c);
  return total;
}

/** The 52 cards in id order. */
export function fullDeck(): Card[] {
  return Array.from({ length: 52 }, (_, i) => i);
}

/** A freshly shuffled deck; `deck[0]` is the top card. */
export function shuffledDeck(rng: Rng): Card[] {
  return rng.shuffle(fullDeck());
}

/** Ascending by rank, then suit — how deadwood is listed. */
export function compareByRank(a: Card, b: Card): number {
  return rankOf(a) - rankOf(b) || suitOf(a) - suitOf(b);
}

/** Grouped by suit, ascending rank within a suit. */
export function compareBySuit(a: Card, b: Card): number {
  return a - b;
}

export function sortByRank(cards: readonly Card[]): Card[] {
  return [...cards].sort(compareByRank);
}

export function sortBySuit(cards: readonly Card[]): Card[] {
  return [...cards].sort(compareBySuit);
}

const SUIT_LETTERS: Record<string, Suit> = {
  s: 0,
  "♠": 0,
  h: 1,
  "♥": 1,
  d: 2,
  "♦": 2,
  c: 3,
  "♣": 3,
};

const RANK_TOKENS: Record<string, number> = {
  a: 1,
  j: 11,
  q: 12,
  k: 13,
  t: 10,
};

/**
 * Parse a compact card token such as `7s`, `10h`, `Td`, `Qc` or `A♠`. Handy for
 * tests and debugging; throws on anything it does not understand.
 */
export function parseCard(token: string): Card {
  const t = token.trim().replace(/︎/g, "").toLowerCase();
  const suitChar = t.slice(-1);
  const rankPart = t.slice(0, -1);
  const suit = SUIT_LETTERS[suitChar];
  const rank = RANK_TOKENS[rankPart] ?? Number(rankPart);
  if (suit === undefined || !Number.isInteger(rank) || rank < 1 || rank > 13) {
    throw new Error(`Cannot parse card "${token}"`);
  }
  return makeCard(suit, rank);
}

/** Parse a whitespace-separated list of card tokens. */
export function parseCards(list: string): Card[] {
  return list
    .split(/[\s,]+/)
    .filter(Boolean)
    .map(parseCard);
}
