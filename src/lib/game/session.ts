import { shuffledDeck } from "./cards";
import { GAME_TARGET } from "./constants";
import { newMatchScore, recordHand, type MatchScore } from "./match";
import { makeRng } from "./rng";
import { applyAction, dealRound, isLegal, other } from "./round";
import type { Action, Card, Difficulty, HandState, MatchContext, Seat } from "./types";

/**
 * A whole game against the CPU: successive hands, the running score and the
 * deal rotation, as one pure reducer. The UI renders it and dispatches; the
 * CPU's moves arrive as ordinary `act` actions for its seat.
 *
 * Two pieces of presentation are modelled as data so the reducer stays pure:
 * the sound to play (`voice`, bumped by `voiceNonce`) and `motion` — which
 * on-screen card a newly appearing card should glide in from.
 */

export const HUMAN: Seat = 0;
export const CPU: Seat = 1;

export type Voice =
  | "deal"
  | "draw"
  | "take"
  | "discard"
  | "pass"
  | "knock"
  | "gin"
  | "undercut"
  | "stalemate"
  | "win"
  | "lose"
  | "reject";

/** Stable keys the motion layer uses to find card elements on screen. */
export const STOCK_KEY = "stock";
export const cardKey = (card: Card) => `c${card}`;
export const backKey = (slot: number) => `b${slot}`;

export interface Motion {
  nonce: number;
  /** Key of a newly shown element → key of the element it should glide in from. */
  from: Record<string, string>;
  /** A fresh deal: ignore old positions and deal every card out of the stock. */
  deal: boolean;
}

export interface Session {
  difficulty: Difficulty;
  seed: number;
  /** 1-based number of the hand in progress. */
  handNo: number;
  hand: HandState;
  score: MatchScore;
  motion: Motion;
  voice: Voice | null;
  voiceNonce: number;
}

export type SessionAction = { type: "act"; seat: Seat; action: Action } | { type: "next-hand" };

/** The shuffled deck for a given hand of a seeded game — reproducible. */
export function deckFor(seed: number, handNo: number): Card[] {
  return shuffledDeck(makeRng(`${seed}:hand:${handNo}`));
}

function dealMotion(hand: HandState, nonce: number): Motion {
  const from: Record<string, string> = {};
  for (const c of hand.hands[HUMAN]) from[cardKey(c)] = STOCK_KEY;
  hand.hands[CPU].forEach((_, i) => (from[backKey(i)] = STOCK_KEY));
  for (const c of hand.discard) from[cardKey(c)] = STOCK_KEY;
  return { nonce, from, deal: true };
}

export function createSession(difficulty: Difficulty, seed: number, target = GAME_TARGET): Session {
  const dealer: Seat = makeRng(`${seed}:dealer`).next() < 0.5 ? 0 : 1;
  const hand = dealRound(deckFor(seed, 1), dealer);
  return {
    difficulty,
    seed,
    handNo: 1,
    hand,
    score: newMatchScore(target),
    motion: dealMotion(hand, 1),
    voice: "deal",
    voiceNonce: 1,
  };
}

/** The scoreboard as the CPU sees it. */
export function cpuContext(session: Session): MatchContext {
  return {
    myScore: session.score.scores[CPU],
    oppScore: session.score.scores[HUMAN],
    target: session.score.target,
  };
}

/** Where a card the CPU plays should appear to come from: the middle of its fan. */
function cpuSlot(hand: HandState): number {
  return Math.floor(hand.hands[CPU].length / 2);
}

function motionFor(prev: HandState, next: HandState, seat: Seat, action: Action): Record<string, string> {
  if (seat === HUMAN) {
    if (action.type === "draw-stock" && next.lastDrawn !== null) {
      return { [cardKey(next.lastDrawn)]: STOCK_KEY };
    }
    return {};
  }
  const newSlot = backKey(next.hands[CPU].length - 1);
  switch (action.type) {
    case "draw-stock":
      return { [newSlot]: STOCK_KEY };
    case "draw-discard": {
      const card = prev.discard[prev.discard.length - 1];
      return { [newSlot]: cardKey(card) };
    }
    case "discard":
    case "knock":
      return { [cardKey(action.card)]: backKey(cpuSlot(prev)) };
    default:
      return {};
  }
}

function voiceFor(next: HandState, action: Action, gameOver: Seat | null): Voice {
  if (gameOver !== null) return gameOver === HUMAN ? "win" : "lose";
  if (next.result) {
    switch (next.result.outcome) {
      case "gin":
      case "big-gin":
        return "gin";
      case "undercut":
        return "undercut";
      case "draw":
        return "stalemate";
      default:
        return "knock";
    }
  }
  switch (action.type) {
    case "pass":
      return "pass";
    case "draw-stock":
      return "draw";
    case "draw-discard":
      return "take";
    default:
      return "discard";
  }
}

export function sessionReducer(state: Session, action: SessionAction): Session {
  switch (action.type) {
    case "act": {
      if (!isLegal(state.hand, action.seat, action.action)) {
        return { ...state, voice: "reject", voiceNonce: state.voiceNonce + 1 };
      }
      const hand = applyAction(state.hand, action.seat, action.action);
      const score = hand.result ? recordHand(state.score, hand.result) : state.score;
      return {
        ...state,
        hand,
        score,
        motion: {
          nonce: state.motion.nonce + 1,
          from: motionFor(state.hand, hand, action.seat, action.action),
          deal: false,
        },
        voice: voiceFor(hand, action.action, score.winner),
        voiceNonce: state.voiceNonce + 1,
      };
    }

    case "next-hand": {
      if (state.hand.phase !== "over" || state.score.winner !== null) return state;
      const last = state.hand.result;
      // The deal alternates; a drawn hand is dealt again by the same dealer.
      const dealer = last && last.outcome === "draw" ? state.hand.dealer : other(state.hand.dealer);
      const handNo = state.handNo + 1;
      const hand = dealRound(deckFor(state.seed, handNo), dealer);
      return {
        ...state,
        handNo,
        hand,
        motion: dealMotion(hand, state.motion.nonce + 1),
        voice: "deal",
        voiceNonce: state.voiceNonce + 1,
      };
    }
  }
}
