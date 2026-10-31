import { describe, expect, it } from "vitest";
import {
  cardLabel,
  cardName,
  cardValue,
  fullDeck,
  isRed,
  makeCard,
  parseCard,
  pipTotal,
  rankOf,
  shuffledDeck,
  sortByRank,
  sortBySuit,
  suitOf,
} from "@/lib/game/cards";
import { makeRng } from "@/lib/game/rng";
import { cards, isWholeDeck } from "./helpers";

describe("deck", () => {
  it("has 52 distinct cards, 13 per suit", () => {
    const deck = fullDeck();
    expect(isWholeDeck(deck)).toBe(true);
    for (let s = 0; s < 4; s++) expect(deck.filter((c) => suitOf(c) === s)).toHaveLength(13);
  });

  it("round-trips suit and rank through the compact id", () => {
    for (let s = 0; s < 4; s++) {
      for (let r = 1; r <= 13; r++) {
        const c = makeCard(s, r);
        expect(suitOf(c)).toBe(s);
        expect(rankOf(c)).toBe(r);
      }
    }
  });

  it("shuffles reproducibly from a seed and keeps every card", () => {
    const a = shuffledDeck(makeRng("seed-1"));
    const b = shuffledDeck(makeRng("seed-1"));
    const c = shuffledDeck(makeRng("seed-2"));
    expect(a).toEqual(b);
    expect(a).not.toEqual(c);
    expect(isWholeDeck(a)).toBe(true);
  });
});

describe("deadwood values", () => {
  it("counts aces as 1, pips at face value and court cards as 10", () => {
    expect(cardValue(parseCard("As"))).toBe(1);
    expect(cardValue(parseCard("2h"))).toBe(2);
    expect(cardValue(parseCard("7d"))).toBe(7);
    expect(cardValue(parseCard("10c"))).toBe(10);
    expect(cardValue(parseCard("Js"))).toBe(10);
    expect(cardValue(parseCard("Qh"))).toBe(10);
    expect(cardValue(parseCard("Kd"))).toBe(10);
  });

  it("totals a hand without melding", () => {
    expect(pipTotal(cards("As 5h 10d Kc"))).toBe(26);
  });
});

describe("labels and parsing", () => {
  it("parses short tokens, glyphs and the T shorthand", () => {
    expect(parseCard("10h")).toBe(parseCard("Th"));
    expect(parseCard("A♠")).toBe(makeCard(0, 1));
    expect(parseCard("qd")).toBe(makeCard(2, 12));
    expect(() => parseCard("1x")).toThrow();
    expect(() => parseCard("14s")).toThrow();
  });

  it("labels cards for the screen and for screen readers", () => {
    expect(cardLabel(parseCard("10h"))).toBe("10♥︎");
    expect(cardName(parseCard("Qs"))).toBe("queen of spades");
    expect(isRed(parseCard("3d"))).toBe(true);
    expect(isRed(parseCard("3c"))).toBe(false);
  });

  it("sorts by rank or by suit", () => {
    const hand = cards("Kd 2s 7h 2c As");
    expect(sortByRank(hand)).toEqual(cards("As 2s 2c 7h Kd"));
    expect(sortBySuit(hand)).toEqual(cards("As 2s 7h Kd 2c"));
  });
});
