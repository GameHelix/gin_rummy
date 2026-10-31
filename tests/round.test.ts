import { describe, expect, it } from "vitest";
import { shuffledDeck } from "@/lib/game/cards";
import { makeRng } from "@/lib/game/rng";
import {
  applyAction,
  cardsInPlay,
  dealRound,
  IllegalActionError,
  isLegal,
  legalActions,
  topDiscard,
  viewFor,
} from "@/lib/game/round";
import type { Action, HandState } from "@/lib/game/types";
import { card, cards, isWholeDeck, stateWith } from "./helpers";

const deal = (seed: string, dealer: 0 | 1 = 1) => dealRound(shuffledDeck(makeRng(seed)), dealer);

describe("dealing", () => {
  it("deals ten each, turns one card up and stocks the other 31", () => {
    const s = deal("deal");
    expect(s.hands[0]).toHaveLength(10);
    expect(s.hands[1]).toHaveLength(10);
    expect(s.discard).toHaveLength(1);
    expect(s.stock).toHaveLength(31);
    expect(isWholeDeck(cardsInPlay(s))).toBe(true);
  });

  it("opens with the upcard offered to the non-dealer", () => {
    const s = deal("deal", 1);
    expect(s.phase).toBe("offer");
    expect(s.turn).toBe(0);
    expect(legalActions(s)).toEqual([{ type: "draw-discard" }, { type: "pass" }]);
    expect(isLegal(s, 0, { type: "draw-stock" })).toBe(false);
  });

  it("rejects an incomplete deck", () => {
    expect(() => dealRound(shuffledDeck(makeRng("x")).slice(1), 0)).toThrow();
  });
});

describe("the first-turn upcard offer", () => {
  it("lets the non-dealer take the upcard, then the dealer plays on", () => {
    const s0 = deal("offer-take", 1);
    const up = topDiscard(s0)!;
    const s1 = applyAction(s0, 0, { type: "draw-discard" });
    expect(s1.phase).toBe("discard");
    expect(s1.hands[0]).toContain(up);
    expect(s1.takenFromDiscard).toBe(up);
    // The upcard may not go straight back.
    expect(isLegal(s1, 0, { type: "discard", card: up })).toBe(false);
    const other = s1.hands[0].find((c) => c !== up)!;
    const s2 = applyAction(s1, 0, { type: "discard", card: other });
    expect(s2.turn).toBe(1);
    expect(s2.phase).toBe("draw");
    expect(s2.stockOnly).toBe(false);
    expect(isLegal(s2, 1, { type: "draw-discard" })).toBe(true);
  });

  it("passes the offer to the dealer, who may take it", () => {
    const s0 = deal("offer-pass", 1);
    const s1 = applyAction(s0, 0, { type: "pass" });
    expect(s1.phase).toBe("offer");
    expect(s1.turn).toBe(1);
    const s2 = applyAction(s1, 1, { type: "draw-discard" });
    expect(s2.phase).toBe("discard");
    expect(s2.hands[1]).toHaveLength(11);
    const s3 = applyAction(s2, 1, { type: "discard", card: s2.hands[1][0] });
    // After the dealer's discard the non-dealer draws normally.
    expect(s3.turn).toBe(0);
    expect(s3.phase).toBe("draw");
    expect(s3.stockOnly).toBe(false);
  });

  it("makes the non-dealer open from the stock when both pass", () => {
    const s0 = deal("offer-both", 0);
    const s1 = applyAction(s0, 1, { type: "pass" });
    const s2 = applyAction(s1, 0, { type: "pass" });
    expect(s2.turn).toBe(1);
    expect(s2.phase).toBe("draw");
    expect(s2.stockOnly).toBe(true);
    expect(legalActions(s2)).toEqual([{ type: "draw-stock" }]);
    expect(() => applyAction(s2, 1, { type: "draw-discard" })).toThrow(IllegalActionError);
    const s3 = applyAction(s2, 1, { type: "draw-stock" });
    expect(s3.stockOnly).toBe(false);
    expect(s3.log.map((e) => e.kind)).toEqual(["pass", "pass", "draw"]);
  });
});

describe("turns", () => {
  it("rejects moves out of turn or out of phase", () => {
    const s = deal("turns", 1);
    expect(() => applyAction(s, 1, { type: "pass" })).toThrow(IllegalActionError);
    expect(isLegal(s, 0, { type: "discard", card: s.hands[0][0] })).toBe(false);
    expect(isLegal(s, 0, { type: "big-gin" })).toBe(false);
  });

  it("forbids discarding the card just taken from the pile, in normal play too", () => {
    const s0 = stateWith({
      hands: ["2c 3c 4c 7s 7h 7d 9d 10d Jd Kh", "Ks Qh 8s 6h 2s 3h 9c Jc 4h Ad"],
      discard: "Qd",
      turn: 0,
    });
    const s1 = applyAction(s0, 0, { type: "draw-discard" });
    expect(isLegal(s1, 0, { type: "discard", card: card("Qd") })).toBe(false);
    expect(isLegal(s1, 0, { type: "knock", card: card("Qd") })).toBe(false);
    expect(legalActions(s1).some((a) => "card" in a && a.card === card("Qd"))).toBe(false);
    // Anything else is fine — and throwing the K♥ leaves 9-10-J-Q♦ and no deadwood.
    expect(isLegal(s1, 0, { type: "discard", card: card("Kh") })).toBe(true);
    const s2 = applyAction(s1, 0, { type: "knock", card: card("Kh") });
    expect(s2.result?.outcome).toBe("gin");
  });

  it("keeps all 52 cards on the table through a draw and discard", () => {
    let s: HandState = deal("count", 0);
    s = applyAction(s, 1, { type: "pass" });
    s = applyAction(s, 0, { type: "pass" });
    s = applyAction(s, 1, { type: "draw-stock" });
    expect(isWholeDeck(cardsInPlay(s))).toBe(true);
    s = applyAction(s, 1, { type: "discard", card: s.hands[1][3] });
    expect(isWholeDeck(cardsInPlay(s))).toBe(true);
    expect(s.hands[1]).toHaveLength(10);
  });
});

describe("knocking", () => {
  const discardPhase = (mine: string, theirs = "Ks Qh 8s 6h 2s 3h 9c Jc 4h Ad") =>
    stateWith({ hands: [mine, theirs], phase: "discard", turn: 0 });

  it("allows a knock only when the kept ten cards hold ten or less deadwood", () => {
    // 2-3-4♣, 7-7-7, 9-10-J♦; loose 5♥ and K♠.
    const s = discardPhase("2c 3c 4c 7s 7h 7d 9d 10d Jd 5h Kd");
    expect(isLegal(s, 0, { type: "knock", card: card("Kd") })).toBe(true); // keeps 5
    expect(isLegal(s, 0, { type: "knock", card: card("5h") })).toBe(true); // keeps 10
    expect(isLegal(s, 0, { type: "knock", card: card("2c") })).toBe(false); // breaks a run
    const knocks = legalActions(s).filter((a) => a.type === "knock");
    expect(knocks.map((a) => (a as { card: number }).card).sort()).toEqual(
      cards("Kd 5h").sort()
    );
  });

  it("draws the line at ten: eleven deadwood may not knock", () => {
    const opp = "Kh Qh 8s 6h 9s 3h 9c Jc 4h Ad";
    // 2-3-4♣ and 7-7-7 melded; A♥ 3♠ 2♦ 4♠ stay loose after throwing the K♠: 10.
    const ten = discardPhase("2c 3c 4c 7s 7h 7d Ah 3s 2d 4s Ks", opp);
    expect(isLegal(ten, 0, { type: "knock", card: card("Ks") })).toBe(true);
    // Swap the 4♠ for the 5♥ and the best keep is 11: no knock anywhere.
    const eleven = discardPhase("2c 3c 4c 7s 7h 7d Ah 3s 2d 5h Ks", opp);
    expect(isLegal(eleven, 0, { type: "knock", card: card("Ks") })).toBe(false);
    expect(legalActions(eleven).some((a) => a.type === "knock")).toBe(false);
  });

  it("ends the hand with a scored result and the knock card face down on the pile", () => {
    const s0 = discardPhase("2c 3c 4c 7s 7h 7d 9d 10d Jd 5h Kd");
    const s1 = applyAction(s0, 0, { type: "knock", card: card("Kd") });
    expect(s1.phase).toBe("over");
    expect(s1.result?.outcome).toBe("knock");
    expect(s1.result?.knocker).toBe(0);
    expect(s1.result?.winner).toBe(0);
    expect(s1.result?.points).toBe(63 - 5);
    expect(s1.result?.knockDiscard).toBe(card("Kd"));
    expect(topDiscard(s1)).toBe(card("Kd"));
    expect(isWholeDeck(cardsInPlay(s1))).toBe(true);
    expect(legalActions(s1)).toEqual([]);
  });

  it("calls a zero-deadwood knock gin", () => {
    const s0 = discardPhase("2c 3c 4c 7s 7h 7d 9d 10d Jd Qd Kh");
    const s1 = applyAction(s0, 0, { type: "knock", card: card("Kh") });
    expect(s1.result?.outcome).toBe("gin");
    expect(s1.result?.points).toBe(25 + 63);
  });

  const bigOpp = "Ks Qh 8s 6h 7d 3h 8c Kd 4h Ad";

  it("allows big gin with all eleven melded, ending the hand without a discard", () => {
    const s0 = discardPhase("As 2s 3s 4s 5d 5h 5c 9c 10c Jc Qc", bigOpp);
    expect(isLegal(s0, 0, { type: "big-gin" })).toBe(true);
    const s1 = applyAction(s0, 0, { type: "big-gin" });
    expect(s1.result?.outcome).toBe("big-gin");
    expect(s1.result?.points).toBe(31 + 67);
    expect(s1.hands[0]).toHaveLength(11);
    expect(s1.result?.knockDiscard).toBeNull();
    expect(isWholeDeck(cardsInPlay(s1))).toBe(true);
  });

  it("does not offer big gin when a card is unmelded", () => {
    const s = discardPhase("As 2s 3s 4s 5d 5h 5c 9c 10c Jc Kc", bigOpp);
    expect(isLegal(s, 0, { type: "big-gin" })).toBe(false);
  });
});

describe("a drawn hand", () => {
  // Three stock cards: whoever draws the 50th card leaves two behind.
  const late = () =>
    stateWith({
      hands: ["2c 3c 4c 7s 7h 7d 9d 10d Jd Kh", "Ks Qh 8s 6h 2s 3h 9c Jc 4h Ad"],
      stock: "5h 6d 8c",
      fill: "discard",
      turn: 0,
    });

  it("is a draw once a discard leaves only two stock cards", () => {
    const s0 = late();
    expect(s0.stock).toHaveLength(3);
    const s1 = applyAction(s0, 0, { type: "draw-stock" });
    expect(s1.stock).toHaveLength(2);
    const s2 = applyAction(s1, 0, { type: "discard", card: card("Kh") });
    expect(s2.phase).toBe("over");
    expect(s2.result?.outcome).toBe("draw");
    expect(s2.result?.winner).toBeNull();
    expect(s2.result?.points).toBe(0);
    expect(isWholeDeck(cardsInPlay(s2))).toBe(true);
  });

  it("still lets the player who drew the 50th card knock instead", () => {
    const s1 = applyAction(late(), 0, { type: "draw-stock" }); // draws the 8♣
    expect(isLegal(s1, 0, { type: "knock", card: card("Kh") })).toBe(true);
    const s2 = applyAction(s1, 0, { type: "knock", card: card("Kh") });
    expect(s2.result?.outcome).toBe("knock");
  });

  it("carries on while three or more cards remain", () => {
    const s0 = stateWith({
      hands: ["2c 3c 4c 7s 7h 7d 9d 10d Jd Kh", "Ks Qh 8s 6h 2s 3h 9c Jc 4h Ad"],
      stock: "5h 6d 8c 10s",
      fill: "discard",
      turn: 0,
    });
    const s1 = applyAction(s0, 0, { type: "draw-stock" });
    const s2 = applyAction(s1, 0, { type: "discard", card: card("Kh") });
    expect(s2.phase).toBe("draw");
    expect(s2.stock).toHaveLength(3);
  });
});

describe("what a seat may see", () => {
  it("shows its own hand and the public record, never the other hand or the stock", () => {
    let s = deal("view", 1);
    s = applyAction(s, 0, { type: "pass" });
    const v = viewFor(s, 1);
    expect(v.hand).toEqual(s.hands[1]);
    expect(v.oppHandSize).toBe(10);
    expect(v.stockCount).toBe(31);
    expect(Object.keys(v).sort()).toEqual(
      [
        "seat",
        "hand",
        "discard",
        "stockCount",
        "dealer",
        "turn",
        "phase",
        "stockOnly",
        "takenFromDiscard",
        "oppHandSize",
        "log",
      ].sort()
    );
    const exposed = new Set([...v.hand, ...v.discard]);
    expect(s.hands[0].some((c) => exposed.has(c))).toBe(false);
  });

  it("only legal actions are listed in every reachable phase", () => {
    let s: HandState = deal("legal-list", 0);
    const rng = makeRng("legal-list-play");
    for (let step = 0; step < 60 && s.phase !== "over"; step++) {
      const options = legalActions(s);
      for (const a of options) expect(isLegal(s, s.turn, a)).toBe(true);
      const plain = options.filter((a: Action) => a.type !== "knock" && a.type !== "big-gin");
      s = applyAction(s, s.turn, rng.pick(plain));
    }
  });
});
