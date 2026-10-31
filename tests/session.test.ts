import { describe, expect, it } from "vitest";
import { cardsInPlay } from "@/lib/game/round";
import {
  backKey,
  cardKey,
  CPU,
  createSession,
  deckFor,
  HUMAN,
  sessionReducer,
  STOCK_KEY,
  type Session,
} from "@/lib/game/session";
import type { HandResult, HandState } from "@/lib/game/types";
import { isWholeDeck } from "./helpers";

/** Force the current hand to end with `result`, as if it had just been played out. */
function finish(s: Session, result: HandResult): Session {
  const hand: HandState = { ...s.hand, phase: "over", result };
  return { ...s, hand };
}

const empty = { melds: [], deadwood: [], deadwoodValue: 0, layoffs: [] };
const drawn: HandResult = {
  outcome: "draw",
  knocker: null,
  winner: null,
  points: 0,
  layouts: [empty, empty],
  knockDiscard: null,
};

describe("a game session", () => {
  it("deals the first hand reproducibly from the seed", () => {
    const a = createSession("medium", 42);
    const b = createSession("medium", 42);
    expect(a.hand).toEqual(b.hand);
    expect(a.handNo).toBe(1);
    expect(isWholeDeck(cardsInPlay(a.hand))).toBe(true);
    expect(deckFor(42, 1)).toEqual(deckFor(42, 1));
    expect(deckFor(42, 1)).not.toEqual(deckFor(42, 2));
    expect(a.voice).toBe("deal");
    expect(a.motion.deal).toBe(true);
  });

  it("deals every card out of the stock on a fresh hand", () => {
    const s = createSession("easy", 7);
    for (const c of s.hand.hands[HUMAN]) expect(s.motion.from[cardKey(c)]).toBe(STOCK_KEY);
    expect(s.motion.from[backKey(9)]).toBe(STOCK_KEY);
  });

  it("applies legal moves and rejects illegal ones without changing the hand", () => {
    const s0 = createSession("easy", 3);
    const actor = s0.hand.turn;
    const bad = sessionReducer(s0, { type: "act", seat: actor, action: { type: "draw-stock" } });
    expect(bad.hand).toBe(s0.hand);
    expect(bad.voice).toBe("reject");
    const ok = sessionReducer(s0, { type: "act", seat: actor, action: { type: "pass" } });
    expect(ok.hand.turn).not.toBe(actor);
    expect(ok.voice).toBe("pass");
  });

  it("glides the CPU's blind draw out of the stock into its fan", () => {
    let s = createSession("easy", 11);
    // Walk both passes so a stock draw is due, whoever deals.
    s = sessionReducer(s, { type: "act", seat: s.hand.turn, action: { type: "pass" } });
    s = sessionReducer(s, { type: "act", seat: s.hand.turn, action: { type: "pass" } });
    const drawer = s.hand.turn;
    s = sessionReducer(s, { type: "act", seat: drawer, action: { type: "draw-stock" } });
    if (drawer === CPU) expect(s.motion.from[backKey(10)]).toBe(STOCK_KEY);
    else expect(s.motion.from[cardKey(s.hand.lastDrawn!)]).toBe(STOCK_KEY);
  });

  it("alternates the deal, but a drawn hand is dealt again by the same dealer", () => {
    const s0 = createSession("medium", 5);
    const firstDealer = s0.hand.dealer;
    const winner: HandResult = { ...drawn, outcome: "knock", knocker: 0, winner: 0, points: 12 };
    const afterWin = finish({ ...s0, score: { ...s0.score } }, winner);
    const s1 = sessionReducer(
      { ...afterWin, score: { ...afterWin.score, scores: [12, 0], handsWon: [1, 0] } },
      { type: "next-hand" }
    );
    expect(s1.handNo).toBe(2);
    expect(s1.hand.dealer).not.toBe(firstDealer);
    const s2 = sessionReducer(finish(s1, drawn), { type: "next-hand" });
    expect(s2.hand.dealer).toBe(s1.hand.dealer);
    expect(s2.handNo).toBe(3);
  });

  it("stops dealing once the game has a winner", () => {
    const s0 = createSession("hard", 9);
    const over = finish({ ...s0, score: { ...s0.score, scores: [104, 20], winner: 0 } }, drawn);
    expect(sessionReducer(over, { type: "next-hand" })).toBe(over);
  });

  it("will not deal the next hand while one is in progress", () => {
    const s0 = createSession("hard", 10);
    expect(sessionReducer(s0, { type: "next-hand" })).toBe(s0);
  });
});
