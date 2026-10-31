import { describe, expect, it } from "vitest";
import { cardValue, shuffledDeck, sortBySuit } from "@/lib/game/cards";
import { BIG_GIN_BONUS, GIN_BONUS, KNOCK_LIMIT, STOCK_FLOOR, UNDERCUT_BONUS } from "@/lib/game/constants";
import { chooseAction } from "@/lib/game/cpu";
import { finalTally } from "@/lib/game/match";
import { isMeld } from "@/lib/game/melds";
import { makeRng } from "@/lib/game/rng";
import { applyAction, cardsInPlay, dealRound, other, viewFor } from "@/lib/game/round";
import { createSession, cpuContext, sessionReducer, type Session } from "@/lib/game/session";
import type { Card, Difficulty, HandState, Seat, SeatLayout } from "@/lib/game/types";
import { isWholeDeck } from "./helpers";

const TIERS: Difficulty[] = ["easy", "medium", "hard"];

/** Every card of a seat's layout, lay-offs included. */
function layoutCards(l: SeatLayout): Card[] {
  return [...l.melds.flatMap((m) => m.cards), ...l.deadwood, ...l.layoffs.map((x) => x.card)];
}

/** Check the table and, once the hand is over, that the settlement adds up. */
function checkState(s: HandState) {
  expect(isWholeDeck(cardsInPlay(s))).toBe(true);
  if (s.phase !== "over") {
    expect(s.hands[s.turn]).toHaveLength(s.phase === "discard" ? 11 : 10);
    expect(s.hands[other(s.turn)]).toHaveLength(10);
    // Only the player who just drew the 50th card ever sees the stock at its floor.
    expect(s.stock.length).toBeGreaterThanOrEqual(s.phase === "discard" ? STOCK_FLOOR : STOCK_FLOOR + 1);
    return;
  }
  const r = s.result!;
  for (const seat of [0, 1] as Seat[]) {
    const l = r.layouts[seat];
    expect(sortBySuit(layoutCards(l))).toEqual(sortBySuit(s.hands[seat]));
    for (const m of l.melds) expect(isMeld(m.cards)).toBe(true);
    expect(l.deadwoodValue).toBe(l.deadwood.reduce((t, c) => t + cardValue(c), 0));
  }
  if (r.outcome === "draw") {
    expect(s.stock).toHaveLength(STOCK_FLOOR);
    expect(r.points).toBe(0);
    return;
  }
  const knocker = r.knocker!;
  const k = r.layouts[knocker];
  const d = r.layouts[other(knocker)];
  expect(k.deadwoodValue).toBeLessThanOrEqual(KNOCK_LIMIT);
  expect(k.layoffs).toEqual([]);
  // Lay-offs land on real knocker melds and keep them valid.
  k.melds.forEach((m, i) => {
    const extra = d.layoffs.filter((x) => x.meld === i).map((x) => x.card);
    expect(isMeld([...m.cards, ...extra])).toBe(true);
  });
  switch (r.outcome) {
    case "gin":
      expect(k.deadwoodValue).toBe(0);
      expect(d.layoffs).toEqual([]);
      expect(r.points).toBe(GIN_BONUS + d.deadwoodValue);
      expect(r.winner).toBe(knocker);
      break;
    case "big-gin":
      expect(s.hands[knocker]).toHaveLength(11);
      expect(k.deadwoodValue).toBe(0);
      expect(r.points).toBe(BIG_GIN_BONUS + d.deadwoodValue);
      break;
    case "knock":
      expect(d.deadwoodValue).toBeGreaterThan(k.deadwoodValue);
      expect(r.points).toBe(d.deadwoodValue - k.deadwoodValue);
      expect(r.winner).toBe(knocker);
      break;
    case "undercut":
      expect(d.deadwoodValue).toBeLessThanOrEqual(k.deadwoodValue);
      expect(r.points).toBe(k.deadwoodValue - d.deadwoodValue + UNDERCUT_BONUS);
      expect(r.winner).toBe(other(knocker));
      break;
  }
}

describe("CPU against CPU", () => {
  it("plays 360 hands to a proper finish with the deck intact at every step", () => {
    const outcomes = new Map<string, number>();
    for (let h = 0; h < 360; h++) {
      const tiers: [Difficulty, Difficulty] = [TIERS[h % 3], TIERS[Math.floor(h / 3) % 3]];
      let s = dealRound(shuffledDeck(makeRng(`sim-${h}`)), (h % 2) as Seat);
      const rng = makeRng(`sim-${h}:cpu`);
      let plies = 0;
      checkState(s);
      while (s.phase !== "over") {
        s = applyAction(s, s.turn, chooseAction(viewFor(s, s.turn), tiers[s.turn], rng));
        checkState(s);
        plies++;
        expect(plies).toBeLessThan(200);
      }
      const o = s.result!.outcome;
      outcomes.set(o, (outcomes.get(o) ?? 0) + 1);
    }
    // A healthy mix: plenty of knocks, some gins and undercuts.
    expect(outcomes.get("knock") ?? 0).toBeGreaterThan(150);
    expect(outcomes.get("gin") ?? 0).toBeGreaterThan(10);
    expect(outcomes.get("undercut") ?? 0).toBeGreaterThan(5);
  }, 120_000);

  it("plays whole games to 100 through the session, tallying them correctly", () => {
    let games = 0;
    for (let g = 0; g < 36; g++) {
      const tiers: [Difficulty, Difficulty] = [TIERS[g % 3], TIERS[Math.floor(g / 3) % 3]];
      let s: Session = createSession(tiers[1], 9000 + g);
      let hands = 0;
      while (s.score.winner === null) {
        if (s.hand.phase === "over") {
          s = sessionReducer(s, { type: "next-hand" });
          hands++;
          expect(hands).toBeLessThan(80);
          continue;
        }
        const seat = s.hand.turn;
        const ctx =
          seat === 1
            ? cpuContext(s)
            : { myScore: s.score.scores[0], oppScore: s.score.scores[1], target: s.score.target };
        const action = chooseAction(viewFor(s.hand, seat), tiers[seat], makeRng(`${g}:${s.voiceNonce}`), ctx);
        s = sessionReducer(s, { type: "act", seat, action });
        expect(s.voice).not.toBe("reject");
        checkState(s.hand);
      }
      const winner = s.score.winner;
      expect(s.score.scores[winner]).toBeGreaterThanOrEqual(100);
      expect(s.score.scores[other(winner)]).toBeLessThan(100);
      // The history replays to the same totals.
      const replay: [number, number] = [0, 0];
      for (const h of s.score.history) {
        if (h.winner !== null) replay[h.winner] += h.points;
        expect(h.totals).toEqual(replay);
      }
      const t = finalTally(s.score);
      const base = (seat: Seat) => t.points[seat] + t.gameBonus[seat] + t.boxBonus[seat];
      expect(t.totals[winner]).toBe(base(winner) * (t.shutout ? 2 : 1));
      expect(t.totals[other(winner)]).toBe(base(other(winner)));
      expect(t.margin).toBeGreaterThan(0);
      games++;
    }
    expect(games).toBe(36);
  }, 120_000);

  it("ranks the tiers: hard beats medium and medium beats easy over many games", () => {
    const play = (a: Difficulty, b: Difficulty, games: number) => {
      let winsA = 0;
      for (let g = 0; g < games; g++) {
        const flip = g % 2 === 1;
        const tiers: [Difficulty, Difficulty] = flip ? [b, a] : [a, b];
        let s = createSession("medium", 4000 + g);
        while (s.score.winner === null) {
          if (s.hand.phase === "over") {
            s = sessionReducer(s, { type: "next-hand" });
            continue;
          }
          const seat = s.hand.turn;
          const ctx = {
            myScore: s.score.scores[seat],
            oppScore: s.score.scores[other(seat)],
            target: s.score.target,
          };
          const action = chooseAction(viewFor(s.hand, seat), tiers[seat], makeRng(`${g}:${s.voiceNonce}`), ctx);
          s = sessionReducer(s, { type: "act", seat, action });
        }
        if (s.score.winner === (flip ? 1 : 0)) winsA++;
      }
      return winsA / games;
    };
    expect(play("medium", "easy", 30)).toBeGreaterThan(0.75);
    // Over 400 games Hard takes about two in three from Medium; a seeded sample
    // of 80 must at least show a clear majority.
    expect(play("hard", "medium", 80)).toBeGreaterThan(0.5);
  }, 120_000);
});
