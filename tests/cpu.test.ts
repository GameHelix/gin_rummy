import { describe, expect, it } from "vitest";
import { shuffledDeck } from "@/lib/game/cards";
import { chooseAction, estimateUndercut, ginOuts, joinsMeld } from "@/lib/game/cpu";
import { feedRisk, readTable } from "@/lib/game/knowledge";
import { bestArrangement, deadwoodAfterEachDiscard } from "@/lib/game/melds";
import { makeRng } from "@/lib/game/rng";
import { applyAction, dealRound, isLegal, other, viewFor } from "@/lib/game/round";
import type {
  Action,
  Difficulty,
  HandState,
  PlayerView,
  PublicEvent,
} from "@/lib/game/types";
import { card, cards } from "./helpers";

const TIERS: Difficulty[] = ["easy", "medium", "hard"];

/** A hand-built view for the CPU in seat 1. */
function view(spec: Omit<Partial<PlayerView>, "hand" | "discard"> & { hand: string; discard?: string }): PlayerView {
  const { hand, discard, ...rest } = spec;
  return {
    seat: 1,
    hand: cards(hand),
    discard: discard ? cards(discard) : [],
    stockCount: 20,
    dealer: 0,
    turn: 1,
    phase: "draw",
    stockOnly: false,
    takenFromDiscard: null,
    oppHandSize: 10,
    log: [],
    ...rest,
  };
}

/** Positions met while CPUs of the given tiers play a hand out, with the seat to move. */
function positions(seed: string, tiers: [Difficulty, Difficulty]): HandState[] {
  const out: HandState[] = [];
  let s = dealRound(shuffledDeck(makeRng(seed)), 0);
  const rng = makeRng(`${seed}:play`);
  while (s.phase !== "over") {
    out.push(s);
    s = applyAction(s, s.turn, chooseAction(viewFor(s, s.turn), tiers[s.turn], rng));
  }
  return out;
}

describe("legality", () => {
  it("only ever chooses legal actions, for every tier in every phase", () => {
    let checked = 0;
    for (let g = 0; g < 12; g++) {
      const tiers: [Difficulty, Difficulty] = [TIERS[g % 3], TIERS[(g + 1) % 3]];
      for (const s of positions(`legal-${g}`, tiers)) {
        for (const tier of TIERS) {
          const a = chooseAction(viewFor(s, s.turn), tier, makeRng(checked));
          expect(isLegal(s, s.turn, a)).toBe(true);
          checked++;
        }
      }
    }
    expect(checked).toBeGreaterThan(300);
  });

  it("refuses to move out of turn", () => {
    const s = dealRound(shuffledDeck(makeRng("oot")), 0);
    expect(() => chooseAction(viewFor(s, 0), "hard", makeRng(1))).toThrow();
  });
});

describe("fair play", () => {
  it("decides identically whatever the hidden cards are", () => {
    // Shuffle the opponent's unknown cards with the stock: everything the CPU
    // may see stays the same, so its choice must not change.
    let compared = 0;
    for (let g = 0; g < 6; g++) {
      for (const s of positions(`fair-${g}`, ["hard", "medium"])) {
        if (s.phase === "offer") continue;
        const me = s.turn;
        const opp = other(me);
        const taken = new Set(
          s.log.filter((e) => e.kind === "take" && e.seat === opp).map((e) => (e as { card: number }).card)
        );
        const hidden = s.hands[opp].filter((c) => !taken.has(c));
        const pool = makeRng(`swap-${compared}`).shuffle([...hidden, ...s.stock]);
        const oppHand = [...s.hands[opp].filter((c) => taken.has(c)), ...pool.slice(0, hidden.length)];
        const alt: HandState = {
          ...s,
          hands: me === 0 ? [s.hands[0], oppHand] : [oppHand, s.hands[1]],
          stock: pool.slice(hidden.length),
        };
        expect(viewFor(alt, me)).toEqual(viewFor(s, me));
        for (const tier of TIERS) {
          const a = chooseAction(viewFor(s, me), tier, makeRng(`d-${compared}`));
          const b = chooseAction(viewFor(alt, me), tier, makeRng(`d-${compared}`));
          expect(b).toEqual(a);
        }
        compared++;
      }
    }
    expect(compared).toBeGreaterThan(60);
  });
});

describe("drawing", () => {
  it("medium and hard take a discard that completes a meld", () => {
    const v = view({ hand: "7s 7h 2c 5d 9h Jc Qd 3s Kh 4d", discard: "Ks 7d" });
    expect(joinsMeld(v.hand, card("7d"))).toBe(true);
    expect(chooseAction(v, "medium", makeRng(1))).toEqual({ type: "draw-discard" });
    expect(chooseAction(v, "hard", makeRng(1))).toEqual({ type: "draw-discard" });
  });

  it("medium and hard leave a useless high card for the stock", () => {
    const v = view({ hand: "7s 7h 2c 3c 4c 9h 10h 3s Ah 4d", discard: "2d Kc" });
    expect(chooseAction(v, "medium", makeRng(2))).toEqual({ type: "draw-stock" });
    expect(chooseAction(v, "hard", makeRng(2))).toEqual({ type: "draw-stock" });
  });

  it("easy draws blind far more often than medium", () => {
    let easyTakes = 0;
    let mediumTakes = 0;
    let spots = 0;
    for (let g = 0; g < 10; g++) {
      for (const s of positions(`blind-${g}`, ["medium", "medium"])) {
        if (s.phase !== "draw" || s.stockOnly) continue;
        const v = viewFor(s, s.turn);
        if (chooseAction(v, "easy", makeRng(spots)).type === "draw-discard") easyTakes++;
        if (chooseAction(v, "medium", makeRng(spots)).type === "draw-discard") mediumTakes++;
        spots++;
      }
    }
    expect(spots).toBeGreaterThan(80);
    expect(easyTakes).toBeLessThan(mediumTakes);
    expect(easyTakes / spots).toBeLessThan(0.15);
  });

  it("hard takes an upcard that makes a set and passes on a stray king", () => {
    const hand = "8s 8h 2c 3c 4c 9d 10d 5s Ah 6h";
    const take = view({ hand, discard: "8d", phase: "offer", dealer: 0 });
    expect(chooseAction(take, "hard", makeRng(3))).toEqual({ type: "draw-discard" });
    const pass = view({ hand, discard: "Kc", phase: "offer", dealer: 0 });
    expect(chooseAction(pass, "hard", makeRng(3))).toEqual({ type: "pass" });
  });

  it("draws from the stock when both players passed the upcard", () => {
    const v = view({ hand: "7s 7h 2c 5d 9h Jc Qd 3s Kh 4d", discard: "7d", stockOnly: true });
    for (const tier of TIERS) expect(chooseAction(v, tier, makeRng(4))).toEqual({ type: "draw-stock" });
  });
});

describe("discarding and knocking", () => {
  it("every tier goes gin when a discard leaves nothing unmelded", () => {
    const v = view({ hand: "2c 3c 4c 7s 7h 7d 9d 10d Jd Qd Kh", phase: "discard" });
    for (const tier of TIERS) {
      expect(chooseAction(v, tier, makeRng(5))).toEqual({ type: "knock", card: card("Kh") });
    }
  });

  it("every tier calls big gin with all eleven melded", () => {
    const v = view({ hand: "As 2s 3s 4s 5d 5h 5c 9c 10c Jc Qc", phase: "discard" });
    for (const tier of TIERS) expect(chooseAction(v, tier, makeRng(6))).toEqual({ type: "big-gin" });
  });

  it("medium discards to the lowest deadwood", () => {
    const v = view({ hand: "2c 3c 4c 7s 7h 7d 9d 10d 2h 3s Kd", phase: "discard", stockCount: 25 });
    const a = chooseAction(v, "medium", makeRng(7));
    const after = deadwoodAfterEachDiscard(v.hand);
    const chosen = v.hand.indexOf((a as { card: number }).card);
    expect(after[chosen]).toBe(Math.min(...after));
  });

  it("medium keeps the connected card when deadwood ties", () => {
    // Throwing the K♦ or the Q♣ leaves the same deadwood; the Q♣ sits next to the J♣.
    const v = view({ hand: "2c 3c 4c 7s 7h 7d Jc Qc 3h 5s Kd", phase: "discard", stockCount: 25 });
    expect(chooseAction(v, "medium", makeRng(8))).toEqual({ type: "discard", card: card("Kd") });
  });

  it("hard will not feed a set the opponent is collecting", () => {
    const log: PublicEvent[] = [
      { kind: "draw", seat: 0, declined: card("4s") },
      { kind: "take", seat: 0, card: card("Jh") },
      { kind: "discard", seat: 0, card: card("2d") },
    ];
    const v = view({
      hand: "2c 3c 4c 6d 7d 8d 9s 9h Ks Jc 3h",
      discard: "4s 2d",
      phase: "discard",
      log,
    });
    const k = readTable(v);
    expect(k.oppHas).toEqual([card("Jh")]);
    expect(feedRisk(card("Jc"), k)).toBeGreaterThan(feedRisk(card("Ks"), k));
    expect(chooseAction(v, "hard", makeRng(9))).toEqual({ type: "discard", card: card("Ks") });
  });

  it("hard plays on for gin when it is ahead, and knocks when it is not", () => {
    // Throwing the K♠ leaves only the A♥: one card from gin with five live outs.
    const v = view({ hand: "2c 3c 4c 7s 7h 7d 9d 10d Jd Ah Ks", phase: "discard", stockCount: 22 });
    const kept = v.hand.filter((c) => c !== card("Ks"));
    expect(ginOuts(kept, readTable(v).unseen)).toBe(5);
    const ahead = chooseAction(v, "hard", makeRng(10), { myScore: 40, oppScore: 10, target: 100 });
    expect(ahead.type).toBe("discard");
    const behind = chooseAction(v, "hard", makeRng(10), { myScore: 10, oppScore: 40, target: 100 });
    expect(behind).toEqual({ type: "knock", card: card("Ks") });
  });

  it("easy holds a ten-point knock early but medium takes it", () => {
    const v = view({ hand: "2c 3c 4c 7s 7h 7d 9d 10d Jd 10s Kh", phase: "discard", stockCount: 28 });
    expect(chooseAction(v, "easy", makeRng(11)).type).toBe("discard");
    expect(chooseAction(v, "medium", makeRng(11))).toEqual({ type: "knock", card: card("Kh") });
  });
});

describe("undercut estimate", () => {
  it("rates the same knock riskier once the opponent has had many draws", () => {
    const melds = bestArrangement(cards("2c 3c 4c 7s 7h 7d 9d 10d Jd 6h")).melds;
    const draws = (n: number): PublicEvent[] =>
      Array.from({ length: n }, (): PublicEvent => ({ kind: "draw", seat: 0, declined: null }));
    const early = readTable(view({ hand: "2c 3c 4c 7s 7h 7d 9d 10d Jd 6h", log: draws(0) }));
    const late = readTable(view({ hand: "2c 3c 4c 7s 7h 7d 9d 10d Jd 6h", log: draws(12) }));
    const a = estimateUndercut(melds, 6, early, makeRng("u"), 200);
    const b = estimateUndercut(melds, 6, late, makeRng("u"), 200);
    expect(a.meanDeadwood).toBeGreaterThan(b.meanDeadwood);
    expect(a.risk).toBeLessThanOrEqual(b.risk);
    expect(a.value).toBeGreaterThan(b.value);
  });
});

describe("a quick self-play check", () => {
  it("finishes hands between every pair of tiers", () => {
    for (const a of TIERS) {
      for (const b of TIERS) {
        const seen = positions(`pair-${a}-${b}`, [a, b]);
        const last = seen[seen.length - 1];
        const action: Action = chooseAction(viewFor(last, last.turn), [a, b][last.turn], makeRng(0));
        expect(isLegal(last, last.turn, action)).toBe(true);
      }
    }
  });
});
