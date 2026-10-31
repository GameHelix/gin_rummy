import { describe, expect, it } from "vitest";
import {
  assignLayoffs,
  bestLayoffPlan,
  deadwoodAfterLayoffs,
  layoffCandidates,
} from "@/lib/game/layoff";
import type { Meld } from "@/lib/game/types";
import { card, cards } from "./helpers";

const run = (list: string): Meld => ({ kind: "run", cards: cards(list) });
const set = (list: string): Meld => ({ kind: "set", cards: cards(list) });

describe("lay-off candidates", () => {
  it("chains outward from both ends of a run through the defender's cards", () => {
    const melds = [run("5s 6s 7s")];
    expect(layoffCandidates(melds, cards("8s 9s 3s 4s Kd"))).toEqual(cards("3s 4s 8s 9s"));
  });

  it("stops at a gap: 9♠ cannot reach 5-6-7♠ without the 8♠", () => {
    expect(layoffCandidates([run("5s 6s 7s")], cards("9s 10s Kd"))).toEqual([]);
  });

  it("offers only the missing fourth card to a set, and nothing to a full four", () => {
    expect(layoffCandidates([set("7s 7h 7d")], cards("7c 8c 6c"))).toEqual(cards("7c"));
    expect(layoffCandidates([set("7s 7h 7d 7c")], cards("8c 6c"))).toEqual([]);
  });

  it("never wraps a run past the king or below the ace", () => {
    expect(layoffCandidates([run("Js Qs Ks")], cards("As 10s"))).toEqual(cards("10s"));
    expect(layoffCandidates([run("As 2s 3s")], cards("Ks 4s"))).toEqual(cards("4s"));
  });
});

describe("lay-off assignment", () => {
  it("chains two cards onto one run", () => {
    expect(assignLayoffs([run("5s 6s 7s")], cards("9s 8s"))).toEqual([
      { card: card("8s"), meld: 0 },
      { card: card("9s"), meld: 0 },
    ]);
  });

  it("rejects a card that only works once its link is laid off too", () => {
    expect(assignLayoffs([run("5s 6s 7s")], cards("9s"))).toBeNull();
  });

  it("spreads cards over several melds of both kinds", () => {
    const melds = [run("2s 3s 4s"), run("8s 9s 10s"), set("Kh Kd Kc")];
    const layoffs = assignLayoffs(melds, cards("5s 6s 7s Ks"));
    expect(layoffs).not.toBeNull();
    expect(layoffs).toHaveLength(4);
    expect(layoffs?.find((l) => l.card === card("Ks"))?.meld).toBe(2);
  });
});

describe("the defender's best lay-off plan", () => {
  it("lays off a chain and keeps the rest", () => {
    const plan = bestLayoffPlan([run("5s 6s 7s")], cards("8s 9s Kd 2c 2h 2d"));
    expect(plan.layoffs.map((l) => l.card).sort()).toEqual(cards("8s 9s").sort());
    expect(plan.arrangement.deadwoodValue).toBe(10);
  });

  it("keeps its own set rather than lay off a card that breaks it", () => {
    // Laying off 7♥ (then 8♥) onto 4-5-6♥ would strand 7♠7♦ (14); keeping
    // 7♥7♠7♦ together leaves only the 8♥.
    const plan = bestLayoffPlan([run("4h 5h 6h")], cards("7h 8h 7s 7d"));
    expect(plan.arrangement.deadwoodValue).toBe(8);
    expect(plan.layoffs).toEqual([]);
  });

  it("lays off onto runs and sets in the same knock", () => {
    const melds = [run("9c 10c Jc"), set("4s 4h 4d")];
    const plan = bestLayoffPlan(melds, cards("8c 7c Qc 4c Kh"));
    expect(plan.layoffs).toHaveLength(4);
    expect(plan.arrangement.deadwood).toEqual(cards("Kh"));
    expect(deadwoodAfterLayoffs(melds, cards("8c 7c Qc 4c Kh"))).toBe(10);
  });

  it("prefers the defender's own meld when lay-offs would save nothing more", () => {
    // 5-6-7♠ could all go onto the knocker's runs, but they are a run anyway.
    const plan = bestLayoffPlan([run("2s 3s 4s"), run("8s 9s 10s")], cards("5s 6s 7s Kd"));
    expect(plan.arrangement.deadwoodValue).toBe(10);
    expect(plan.layoffs).toEqual([]);
  });
});
