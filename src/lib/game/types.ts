/**
 * Core Gin Rummy types — pure data, no React, no DOM.
 *
 * A card is a small integer so hands can be scanned with plain arrays and
 * bitmasks: `id = suit * 13 + (rank - 1)`, with ranks running ace-low from
 * A = 1 to K = 13. That makes "same suit, next rank" simply `id + 1`, which the
 * meld finder leans on.
 */
export type Card = number;

/** ♠ ♥ ♦ ♣, in that order. Hearts and diamonds are the red suits. */
export type Suit = 0 | 1 | 2 | 3;

/** The two players. The engine is symmetric; the UI seats the human at 0. */
export type Seat = 0 | 1;

/** Computer opponent strength tiers. */
export type Difficulty = "easy" | "medium" | "hard";

export type MeldKind = "set" | "run";

/** A set (3–4 of a rank) or a run (3+ consecutive cards of one suit, ace low). */
export interface Meld {
  kind: MeldKind;
  /** Runs ascend by rank; sets are ordered by suit. */
  cards: Card[];
}

/** One way to lay a hand down: its melds plus whatever is left over. */
export interface Arrangement {
  melds: Meld[];
  /** Unmelded cards, sorted by rank then suit. */
  deadwood: Card[];
  /** A = 1, 2–10 at face value, J/Q/K = 10. */
  deadwoodValue: number;
}

/**
 * Where a hand (one deal) stands:
 * - `offer`: the first-turn upcard offer — the seat to move may take it or pass;
 * - `draw`: the seat to move draws from the stock or the discard pile;
 * - `discard`: the seat to move holds 11 cards and must discard (or knock);
 * - `over`: settled; `result` says how.
 */
export type Phase = "offer" | "draw" | "discard" | "over";

/** Everything a player can do. Legality depends on the phase — see `round.ts`. */
export type Action =
  /** Decline the upcard during the first-turn offer. */
  | { type: "pass" }
  | { type: "draw-stock" }
  /** Take the top discard; during the offer this takes the upcard. */
  | { type: "draw-discard" }
  | { type: "discard"; card: Card }
  /** Discard `card` face down and lay down: a gin when nothing is left unmelded. */
  | { type: "knock"; card: Card }
  /** All eleven cards melded right after the draw: ends the hand without a discard. */
  | { type: "big-gin" };

/**
 * What both players saw happen. The computer opponent reads only this log, its
 * own cards and the face-up pile — never the other hand or the stock order.
 */
export type PublicEvent =
  /** `seat` declined the upcard `card`. */
  | { kind: "pass"; seat: Seat; card: Card }
  /** `seat` took `card` from the discard pile (the upcard included). */
  | { kind: "take"; seat: Seat; card: Card }
  /** `seat` drew blind from the stock, leaving `declined` on the discard pile. */
  | { kind: "draw"; seat: Seat; declined: Card | null }
  | { kind: "discard"; seat: Seat; card: Card };

/** How a hand ended. A knock that ends with the defender at or below the knocker is an undercut. */
export type Outcome = "knock" | "gin" | "big-gin" | "undercut" | "draw";

/** A defender's card added to one of the knocker's melds; `meld` indexes the knocker's melds. */
export interface Layoff {
  card: Card;
  meld: number;
}

/** One seat's cards as laid on the table at the end of a hand. */
export interface SeatLayout {
  melds: Meld[];
  /** Unmelded cards that still count (after any lay-offs). */
  deadwood: Card[];
  deadwoodValue: number;
  /** Cards this seat laid off onto the knocker's melds (defender only). */
  layoffs: Layoff[];
}

export interface HandResult {
  outcome: Outcome;
  /** Who knocked or went gin; `null` for a drawn hand. */
  knocker: Seat | null;
  /** Who scores; `null` for a drawn hand. */
  winner: Seat | null;
  points: number;
  layouts: [SeatLayout, SeatLayout];
  /** The card the knocker discarded face down; `null` for big gin and draws. */
  knockDiscard: Card | null;
}

/**
 * The complete, immutable state of one hand. Every rule function returns a
 * fresh object; nothing is mutated in place.
 */
export interface HandState {
  hands: [Card[], Card[]];
  /** Face-down stock; the top card is the last element. */
  stock: Card[];
  /** Face-up discard pile; the top card is the last element. */
  discard: Card[];
  dealer: Seat;
  /** The seat that must act next (meaningless once `phase` is `over`). */
  turn: Seat;
  phase: Phase;
  /** Both players passed the upcard: the non-dealer's first draw must come from the stock. */
  stockOnly: boolean;
  /** The card taken from the discard pile this turn — it may not be thrown straight back. */
  takenFromDiscard: Card | null;
  /** The card most recently drawn by the seat to move, for highlighting. */
  lastDrawn: Card | null;
  log: PublicEvent[];
  result: HandResult | null;
}

/**
 * The slice of a hand one seat is allowed to know. The CPU decides from this
 * alone, which is what keeps it honest.
 */
export interface PlayerView {
  seat: Seat;
  hand: readonly Card[];
  discard: readonly Card[];
  stockCount: number;
  dealer: Seat;
  turn: Seat;
  phase: Phase;
  stockOnly: boolean;
  takenFromDiscard: Card | null;
  oppHandSize: number;
  log: readonly PublicEvent[];
}

/** The scoreboard around a hand, which the hardest CPU weighs when it plays for gin. */
export interface MatchContext {
  myScore: number;
  oppScore: number;
  target: number;
}
