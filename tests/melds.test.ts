import { describe, expect, it } from "vitest";
import { cardValue, fullDeck, makeCard, sortBySuit } from "@/lib/game/cards";
import {
  allBestArrangements,
  bestArrangement,
  bestDeadwoodAfterDiscard,
  candidateMelds,
  deadwoodAfterEachDiscard,
  deadwoodOf,
  isMeld,
  isRun,
  isSet,
} from "@/lib/game/melds";
import { makeRng } from "@/lib/game/rng";
import type { Arrangement, Card } from "@/lib/game/types";
import { card, cards } from "./helpers";

/**
 * A deliberately naive reference: try every meld containing the first card
 * (any subset of the rest), or call it deadwood, and recurse. Exponential, but
 * independent of the bitmask solver it checks.
 */
function bruteDeadwood(hand: readonly Card[]): number {
  if (hand.length === 0) return 0;
  const [first, ...rest] = hand;
  let best = cardValue(first) + bruteDeadwood(rest);
  for (let mask = 1; mask < 1 << rest.length; mask++) {
    const picked = rest.filter((_, i) => mask & (1 << i));
    if (picked.length < 2 || !isMeld([first, ...picked])) continue;
    best = Math.min(best, bruteDeadwood(rest.filter((_, i) => !(mask & (1 << i)))));
  }
  return best;
}

/** Every card exactly once across melds and deadwood, every meld valid. */
function expectConsistent(hand: readonly Card[], a: Arrangement) {
  const laid = [...a.melds.flatMap((m) => m.cards), ...a.deadwood];
  expect(sortBySuit(laid)).toEqual(sortBySuit(hand));
  for (const m of a.melds) expect(isMeld(m.cards)).toBe(true);
  expect(a.deadwoodValue).toBe(a.deadwood.reduce((s, c) => s + cardValue(c), 0));
}

const meldStrings = (a: Arrangement) =>
  a.melds.map((m) => m.cards.map((c) => c).join(",")).sort();

describe("meld validity", () => {
  it("accepts sets of three or four of a rank only", () => {
    expect(isSet(cards("7s 7h 7d"))).toBe(true);
    expect(isSet(cards("7s 7h 7d 7c"))).toBe(true);
    expect(isSet(cards("7s 7h"))).toBe(false);
    expect(isSet(cards("7s 7h 8d"))).toBe(false);
  });

  it("accepts same-suit consecutive runs of three or more, in any order", () => {
    expect(isRun(cards("5s 6s 7s"))).toBe(true);
    expect(isRun(cards("7s 5s 6s 8s"))).toBe(true);
    expect(isRun(cards("5s 6h 7s"))).toBe(false);
    expect(isRun(cards("5s 6s 8s"))).toBe(false);
    expect(isRun(cards("5s 6s"))).toBe(false);
  });

  it("plays the ace low only: A-2-3 is a run, Q-K-A and K-A-2 are not", () => {
    expect(isRun(cards("As 2s 3s"))).toBe(true);
    expect(isRun(cards("Qs Ks As"))).toBe(false);
    expect(isRun(cards("Ks As 2s"))).toBe(false);
    expect(deadwoodOf(cards("Qs Ks As"))).toBe(21);
    expect(deadwoodOf(cards("Ks As 2s"))).toBe(13);
    expect(deadwoodOf(cards("As 2s 3s"))).toBe(0);
  });

  it("lists every candidate meld, including the three-card subsets of a four", () => {
    const four = candidateMelds(cards("9s 9h 9d 9c"));
    expect(four).toHaveLength(5);
    const run = candidateMelds(cards("3d 4d 5d 6d"));
    // 3-4-5, 4-5-6 and 3-4-5-6.
    expect(run).toHaveLength(3);
  });
});

describe("optimal melding on tricky overlaps", () => {
  it("7♠7♥7♦ against 5♠6♠7♠8♠: the four-card run wins", () => {
    const hand = cards("7s 7h 7d 5s 6s 8s");
    const a = bestArrangement(hand);
    expect(a.deadwoodValue).toBe(14);
    expect(a.melds).toHaveLength(1);
    expect(a.melds[0]).toEqual({ kind: "run", cards: cards("5s 6s 7s 8s") });
    expect(a.deadwood).toEqual(cards("7h 7d"));
    expectConsistent(hand, a);
  });

  it("splits a four of a kind so the spare card can finish a run", () => {
    const hand = cards("7s 7h 7d 7c 5s 6s 8s");
    const a = bestArrangement(hand);
    expect(a.deadwoodValue).toBe(0);
    expect(meldStrings(a)).toEqual(
      meldStrings({
        melds: [
          { kind: "set", cards: cards("7h 7d 7c") },
          { kind: "run", cards: cards("5s 6s 7s 8s") },
        ],
        deadwood: [],
        deadwoodValue: 0,
      })
    );
  });

  it("beats the greedy 'take the four of a kind first' trap", () => {
    // Greedy: 7777 + 8♦9♦10♦ leaves 5♥6♥8♥ = 19. Optimal melds everything.
    const hand = cards("7s 7h 7d 7c 5h 6h 8h 8d 9d 10d");
    expect(deadwoodOf(hand)).toBe(0);
    expectConsistent(hand, bestArrangement(hand));
  });

  it("prefers two runs over a four of a kind when that leaves less", () => {
    // 8♠9♠10♠ + 8♥9♥10♥ leaves 8♦8♣ (16); 8888 leaves 38; mixed leaves 19.
    expect(deadwoodOf(cards("8s 8h 8d 8c 9s 10s 9h 10h"))).toBe(16);
  });

  it("uses an ace in a set when the rest of the suit still runs", () => {
    // A♥2♥3♥4♥ alone leaves A♣A♦ (2); A♥A♣A♦ + 2-3-4♥ leaves nothing.
    expect(deadwoodOf(cards("Ah 2h 3h 4h Ac Ad"))).toBe(0);
  });

  it("melds a full ten-card run", () => {
    expect(deadwoodOf(cards("As 2s 3s 4s 5s 6s 7s 8s 9s 10s"))).toBe(0);
  });

  it("returns every equally good arrangement", () => {
    const options = allBestArrangements(cards("5s 6s 7s 8s 9s 10s"));
    expect(options.every((o) => o.deadwoodValue === 0)).toBe(true);
    const shapes = options.map((o) => o.melds.map((m) => m.cards.length).join("+")).sort();
    expect(shapes).toEqual(["3+3", "6"]);

    // 6♥7♥8♥ vs 7♥7♠7♦: both leave 14, with different leftovers.
    const tie = allBestArrangements(cards("6h 7h 8h 7s 7d"));
    expect(tie).toHaveLength(2);
    expect(tie.map((o) => o.melds[0].kind).sort()).toEqual(["run", "set"]);
  });

  it("matches a brute-force solver on random and meld-dense hands", () => {
    const rng = makeRng("melds-vs-brute");
    const dense = fullDeck().filter((c) => {
      const r = (c % 13) + 1;
      return r >= 4 && r <= 9;
    });
    for (let i = 0; i < 160; i++) {
      const pool = i % 2 === 0 ? fullDeck() : dense;
      const hand = rng.shuffle([...pool]).slice(0, 10);
      const a = bestArrangement(hand);
      expect(a.deadwoodValue).toBe(bruteDeadwood(hand));
      expectConsistent(hand, a);
    }
  });
});

describe("deadwood after a discard", () => {
  it("reports the deadwood left by each possible discard", () => {
    const hand = cards("5s 6s 7s 7h 7d 7c Kd Qh 2c 3c 9h");
    const after = deadwoodAfterEachDiscard(hand);
    hand.forEach((c, i) => {
      expect(after[i]).toBe(deadwoodOf(hand.filter((x) => x !== c)));
    });
  });

  it("finds the best discard and honours a forbidden card", () => {
    const hand = cards("5s 6s 7s 7h 7d 7c 2c 3c 4c 9h Kd");
    // Throw the K♦: 9♥ is left.
    expect(bestDeadwoodAfterDiscard(hand)).toBe(9);
    // If the K♦ was just taken from the pile, the 9♥ must go instead.
    expect(bestDeadwoodAfterDiscard(hand, card("Kd"))).toBe(10);
  });

  it("knows every card's value in an eleven-card big gin", () => {
    const hand = cards("As 2s 3s 4s 5d 5h 5c 9c 10c Jc Qc");
    expect(deadwoodOf(hand)).toBe(0);
    // Dropping an end of the 4-runs keeps it at zero; dropping a set card breaks the set.
    expect(bestDeadwoodAfterDiscard(hand)).toBe(0);
    expect(deadwoodOf(hand.filter((c) => c !== makeCard(1, 5)))).toBe(10);
  });
});
