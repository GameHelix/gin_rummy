import { describe, expect, it } from "vitest";
import { discardChoices, lastMoveBy, layoutHand, moveCard } from "@/lib/game/display";
import { card, cards, stateWith } from "./helpers";

describe("laying the hand out", () => {
  const hand = cards("Kd 7s 2c 7h 3c 9h 4c 7d Qs 5h");

  it("groups melds first and trails the deadwood in meld mode", () => {
    const l = layoutHand(hand, "melds", []);
    expect(l.groups.map((g) => g.cards.map((c) => c.card))).toEqual([
      cards("2c 3c 4c"),
      cards("7s 7h 7d"),
      cards("5h 9h Qs Kd"),
    ]);
    expect(l.groups[2].cards.every((c) => c.meld === -1)).toBe(true);
    expect(l.order).toHaveLength(10);
    expect(l.arrangement.deadwoodValue).toBe(5 + 9 + 10 + 10);
  });

  it("sorts by rank or suit in one row, still tagging meld membership", () => {
    const byRank = layoutHand(hand, "rank", []);
    expect(byRank.groups).toHaveLength(1);
    expect(byRank.order).toEqual(cards("2c 3c 4c 5h 7s 7h 7d 9h Qs Kd"));
    const sevens = byRank.groups[0].cards.filter((c) => c.card % 13 === 6);
    expect(new Set(sevens.map((c) => c.meld)).size).toBe(1);
    expect(sevens[0].meld).toBeGreaterThanOrEqual(0);
    expect(layoutHand(hand, "suit", []).order).toEqual(cards("7s Qs 5h 7h 9h 7d Kd 2c 3c 4c"));
  });

  it("keeps a hand-arranged order, drops cards no longer held and appends new ones", () => {
    const custom = cards("Qs 2c Ah Kd");
    const l = layoutHand(hand, "custom", custom);
    expect(l.order.slice(0, 3)).toEqual(cards("Qs 2c Kd"));
    expect(l.order).toHaveLength(10);
    expect(l.order).not.toContain(card("Ah"));
  });

  it("moves a dragged card after a card to its right and before one to its left", () => {
    const order = cards("As 2s 3s 4s");
    expect(moveCard(order, card("As"), card("3s"))).toEqual(cards("2s 3s As 4s"));
    expect(moveCard(order, card("4s"), card("2s"))).toEqual(cards("As 4s 2s 3s"));
    expect(moveCard(order, card("2s"), card("2s"))).toEqual(order);
  });
});

describe("what the buttons offer", () => {
  it("offers a knock only when the best discard leaves ten or less", () => {
    // Melds 2-3-4♣ and 7-7-7; the best discard (a ten-pointer) still leaves 27.
    const c = discardChoices(cards("2c 3c 4c 7s 7h 7d 9h 5h Qs Kd 3d"), null);
    expect(c.best).toBe(9 + 5 + 10 + 3);
    expect(c.knock).toBeNull();
    const tight = discardChoices(cards("2c 3c 4c 7s 7h 7d 9d 10d Jd 5h Kd"), null);
    expect(tight.knock).toEqual({ card: card("Kd"), deadwood: 5 });
    expect(tight.gin).toBeNull();
    expect(tight.bigGin).toBe(false);
  });

  it("finds gin and big gin, and never suggests the card just taken", () => {
    const gin = discardChoices(cards("2c 3c 4c 7s 7h 7d 9d 10d Jd Qd Kh"), null);
    expect(gin.gin).toBe(card("Kh"));
    const blocked = discardChoices(cards("2c 3c 4c 7s 7h 7d 9d 10d Jd Qd Kh"), card("Kh"));
    expect(blocked.gin).toBeNull();
    expect(blocked.after.has(card("Kh"))).toBe(false);
    expect(discardChoices(cards("As 2s 3s 4s 5d 5h 5c 9c 10c Jc Qc"), null).bigGin).toBe(true);
  });
});

describe("putting the last move into words", () => {
  const names = ["You", "CPU"] as const;

  it("joins a draw and its discard into one sentence", () => {
    const s = stateWith({
      hands: ["2c 3c 4c 7s 7h 7d 9d 10d Jd Kh", "Ks Qh 8s 6h 2s 3h 9c Jc 4h Ad"],
      log: [
        { kind: "pass", seat: 0, card: card("5d") },
        { kind: "pass", seat: 1, card: card("5d") },
        { kind: "draw", seat: 0, declined: card("5d") },
        { kind: "discard", seat: 0, card: card("Qs") },
        { kind: "take", seat: 1, card: card("Qs") },
        { kind: "discard", seat: 1, card: card("2d") },
      ],
    });
    expect(lastMoveBy(s, 0, names)).toBe("You drew and discarded the Q♠︎");
    expect(lastMoveBy(s, 1, names)).toBe("CPU took the Q♠︎ and discarded the 2♦︎");
  });

  it("reports a pass on its own, and nothing before the first move", () => {
    const s = stateWith({
      hands: ["2c 3c 4c 7s 7h 7d 9d 10d Jd Kh", "Ks Qh 8s 6h 2s 3h 9c Jc 4h Ad"],
      log: [{ kind: "pass", seat: 1, card: card("5d") }],
    });
    expect(lastMoveBy(s, 1, names)).toBe("CPU passed on the 5♦︎");
    expect(lastMoveBy(s, 0, names)).toBeNull();
  });
});
