import { describe, expect, it } from "vitest";
import { finalTally, newMatchScore, recordHand, type MatchScore } from "@/lib/game/match";
import type { HandResult, Outcome, Seat } from "@/lib/game/types";

const empty = { melds: [], deadwood: [], deadwoodValue: 0, layoffs: [] };

function result(winner: Seat | null, points: number, outcome: Outcome = "knock"): HandResult {
  return {
    outcome: winner === null ? "draw" : outcome,
    knocker: winner,
    winner,
    points: winner === null ? 0 : points,
    layouts: [empty, empty],
    knockDiscard: null,
  };
}

function play(...hands: [Seat | null, number][]): MatchScore {
  return hands.reduce((score, [w, p]) => recordHand(score, result(w, p)), newMatchScore());
}

describe("running score", () => {
  it("adds each hand's points to its winner and keeps a history", () => {
    const s = play([0, 18], [1, 30], [0, 7]);
    expect(s.scores).toEqual([25, 30]);
    expect(s.handsWon).toEqual([2, 1]);
    expect(s.history.map((h) => h.totals)).toEqual([
      [18, 0],
      [18, 30],
      [25, 30],
    ]);
    expect(s.winner).toBeNull();
  });

  it("records a drawn hand without scoring it", () => {
    const s = play([0, 10], [null, 0]);
    expect(s.scores).toEqual([10, 0]);
    expect(s.handsWon).toEqual([1, 0]);
    expect(s.history[1]).toMatchObject({ hand: 2, outcome: "draw", winner: null, points: 0 });
  });

  it("ends the game as soon as someone reaches 100", () => {
    const s = play([1, 60], [0, 20], [1, 40]);
    expect(s.scores).toEqual([20, 100]);
    expect(s.winner).toBe(1);
    expect(() => recordHand(s, result(0, 5))).toThrow();
  });
});

describe("final tally", () => {
  it("adds the game bonus and 25 per hand won to both players", () => {
    const s = play([0, 40], [1, 20], [0, 35], [0, 30]);
    const t = finalTally(s);
    expect(t.winner).toBe(0);
    expect(t.points).toEqual([105, 20]);
    expect(t.gameBonus).toEqual([100, 0]);
    expect(t.boxBonus).toEqual([75, 25]);
    expect(t.shutout).toBe(false);
    expect(t.totals).toEqual([280, 45]);
    expect(t.margin).toBe(235);
  });

  it("doubles everything for a shutout", () => {
    const s = play([1, 60], [null, 0], [1, 45]);
    const t = finalTally(s);
    expect(t.winner).toBe(1);
    expect(t.shutout).toBe(true);
    // (105 points + 100 game + 2 × 25 boxes) × 2.
    expect(t.totals).toEqual([0, 510]);
    expect(t.margin).toBe(510);
  });

  it("is not available before the game ends", () => {
    expect(() => finalTally(play([0, 50]))).toThrow();
  });
});
