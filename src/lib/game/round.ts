import { HAND_SIZE, KNOCK_LIMIT, STOCK_FLOOR } from "./constants";
import { bestArrangement, deadwoodAfterEachDiscard, deadwoodOf } from "./melds";
import { layoutOf, settleBigGin, settleKnock, type Settlement } from "./scoring";
import type { Action, Card, HandResult, HandState, PlayerView, Seat } from "./types";

/**
 * The turn state machine for one hand (one deal).
 *
 *   offer ──take──▶ discard ──discard──▶ draw (other seat) ──draw──▶ discard …
 *     │                 │
 *     └─pass─▶ offer (dealer) ─pass─▶ draw (non-dealer, stock only)
 *                       └──knock / gin / big gin──▶ over
 *
 * The non-dealer is offered the upcard first; if they pass, the dealer may take
 * it; if both pass, the non-dealer opens by drawing from the stock. A card
 * taken from the discard pile may not be thrown straight back. When a discard
 * leaves only two cards in the stock and nobody knocked, the hand is a draw.
 *
 * Every function is pure: states go in, fresh states come out.
 */

export class IllegalActionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "IllegalActionError";
  }
}

export function other(seat: Seat): Seat {
  return seat === 0 ? 1 : 0;
}

/**
 * Deal a hand from a full deck (`deck[0]` is the top): ten cards each,
 * alternating from the non-dealer, then the upcard, then the stock.
 */
export function dealRound(deck: readonly Card[], dealer: Seat): HandState {
  if (deck.length !== 52 || new Set(deck).size !== 52) {
    throw new Error("A deal needs a complete 52-card deck");
  }
  const first = other(dealer);
  const hands: [Card[], Card[]] = [[], []];
  for (let i = 0; i < HAND_SIZE * 2; i++) hands[i % 2 === 0 ? first : dealer].push(deck[i]);
  return {
    hands,
    stock: deck.slice(HAND_SIZE * 2 + 1).reverse(),
    discard: [deck[HAND_SIZE * 2]],
    dealer,
    turn: first,
    phase: "offer",
    stockOnly: false,
    takenFromDiscard: null,
    lastDrawn: null,
    log: [],
    result: null,
  };
}

export function topDiscard(state: HandState): Card | null {
  return state.discard.length > 0 ? state.discard[state.discard.length - 1] : null;
}

/** Is a regular discard of `card` allowed right now (ignoring whose turn it is)? */
function canDiscard(state: HandState, card: Card): boolean {
  return (
    state.phase === "discard" &&
    state.hands[state.turn].includes(card) &&
    card !== state.takenFromDiscard
  );
}

/** Is `action` legal for `seat` in `state`? */
export function isLegal(state: HandState, seat: Seat, action: Action): boolean {
  if (state.phase === "over" || seat !== state.turn) return false;
  switch (action.type) {
    case "pass":
      return state.phase === "offer";
    case "draw-discard":
      return (
        (state.phase === "offer" || (state.phase === "draw" && !state.stockOnly)) &&
        state.discard.length > 0
      );
    case "draw-stock":
      return state.phase === "draw" && state.stock.length > STOCK_FLOOR;
    case "discard":
      return canDiscard(state, action.card);
    case "knock": {
      if (!canDiscard(state, action.card)) return false;
      const rest = state.hands[seat].filter((c) => c !== action.card);
      return deadwoodOf(rest) <= KNOCK_LIMIT;
    }
    case "big-gin":
      return state.phase === "discard" && deadwoodOf(state.hands[seat]) === 0;
  }
}

/** Every legal action for the seat to move. */
export function legalActions(state: HandState): Action[] {
  switch (state.phase) {
    case "over":
      return [];
    case "offer":
      return [{ type: "draw-discard" }, { type: "pass" }];
    case "draw": {
      const out: Action[] = [];
      if (state.stock.length > STOCK_FLOOR) out.push({ type: "draw-stock" });
      if (!state.stockOnly && state.discard.length > 0) out.push({ type: "draw-discard" });
      return out;
    }
    case "discard": {
      const hand = state.hands[state.turn];
      const after = deadwoodAfterEachDiscard(hand);
      const out: Action[] = [];
      hand.forEach((card, i) => {
        if (card === state.takenFromDiscard) return;
        out.push({ type: "discard", card });
        if (after[i] <= KNOCK_LIMIT) out.push({ type: "knock", card });
      });
      if (deadwoodOf(hand) === 0) out.push({ type: "big-gin" });
      return out;
    }
  }
}

function withHand(hands: [Card[], Card[]], seat: Seat, cards: Card[]): [Card[], Card[]] {
  return seat === 0 ? [cards, hands[1]] : [hands[0], cards];
}

function resultFrom(settlement: Settlement, knocker: Seat, knockDiscard: Card | null): HandResult {
  const defender = other(knocker);
  const layouts = [settlement.knocker, settlement.defender];
  if (knocker === 1) layouts.reverse();
  return {
    outcome: settlement.outcome,
    knocker,
    winner: settlement.winner === "knocker" ? knocker : defender,
    points: settlement.points,
    layouts: [layouts[0], layouts[1]],
    knockDiscard,
  };
}

function drawnResult(hands: [Card[], Card[]]): HandResult {
  return {
    outcome: "draw",
    knocker: null,
    winner: null,
    points: 0,
    layouts: [layoutOf(bestArrangement(hands[0])), layoutOf(bestArrangement(hands[1]))],
    knockDiscard: null,
  };
}

/**
 * Apply `action` for `seat`, returning the next state. Throws
 * `IllegalActionError` for anything the rules do not allow.
 */
export function applyAction(state: HandState, seat: Seat, action: Action): HandState {
  if (!isLegal(state, seat, action)) {
    throw new IllegalActionError(`Illegal ${action.type} for seat ${seat} in phase ${state.phase}`);
  }
  const hand = state.hands[seat];

  switch (action.type) {
    case "pass": {
      const card = topDiscard(state) as Card;
      const log = [...state.log, { kind: "pass" as const, seat, card }];
      if (seat !== state.dealer) {
        // The non-dealer passed: the dealer gets the same offer.
        return { ...state, turn: state.dealer, log };
      }
      // Both passed: the non-dealer must open from the stock.
      return { ...state, turn: other(seat), phase: "draw", stockOnly: true, log };
    }

    case "draw-discard": {
      const card = topDiscard(state) as Card;
      return {
        ...state,
        hands: withHand(state.hands, seat, [...hand, card]),
        discard: state.discard.slice(0, -1),
        phase: "discard",
        stockOnly: false,
        takenFromDiscard: card,
        lastDrawn: card,
        log: [...state.log, { kind: "take", seat, card }],
      };
    }

    case "draw-stock": {
      const card = state.stock[state.stock.length - 1];
      return {
        ...state,
        hands: withHand(state.hands, seat, [...hand, card]),
        stock: state.stock.slice(0, -1),
        phase: "discard",
        stockOnly: false,
        takenFromDiscard: null,
        lastDrawn: card,
        log: [...state.log, { kind: "draw", seat, declined: topDiscard(state) }],
      };
    }

    case "discard": {
      const rest = hand.filter((c) => c !== action.card);
      const hands = withHand(state.hands, seat, rest);
      const next: HandState = {
        ...state,
        hands,
        discard: [...state.discard, action.card],
        turn: other(seat),
        phase: "draw",
        takenFromDiscard: null,
        lastDrawn: null,
        log: [...state.log, { kind: "discard", seat, card: action.card }],
      };
      if (state.stock.length <= STOCK_FLOOR) {
        return { ...next, phase: "over", result: drawnResult(hands) };
      }
      return next;
    }

    case "knock": {
      const rest = hand.filter((c) => c !== action.card);
      const settlement = settleKnock(rest, state.hands[other(seat)]);
      return {
        ...state,
        hands: withHand(state.hands, seat, rest),
        // The knock card goes face down on the pile; it keeps the deck whole.
        discard: [...state.discard, action.card],
        phase: "over",
        takenFromDiscard: null,
        result: resultFrom(settlement, seat, action.card),
      };
    }

    case "big-gin": {
      const settlement = settleBigGin(hand, state.hands[other(seat)]);
      return {
        ...state,
        phase: "over",
        takenFromDiscard: null,
        result: resultFrom(settlement, seat, null),
      };
    }
  }
}

/** What `seat` may legitimately know: its own cards and the public record. */
export function viewFor(state: HandState, seat: Seat): PlayerView {
  return {
    seat,
    hand: state.hands[seat],
    discard: state.discard,
    stockCount: state.stock.length,
    dealer: state.dealer,
    turn: state.turn,
    phase: state.phase,
    stockOnly: state.stockOnly,
    takenFromDiscard: seat === state.turn ? state.takenFromDiscard : null,
    oppHandSize: state.hands[other(seat)].length,
    log: state.log,
  };
}

/** Every card on the table — always the full deck, once each. */
export function cardsInPlay(state: HandState): Card[] {
  return [...state.hands[0], ...state.hands[1], ...state.stock, ...state.discard];
}
