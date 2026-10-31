import { describe, expect, it } from "vitest";
import { knockVerdict, settleBigGin, settleKnock } from "@/lib/game/scoring";
import { cards } from "./helpers";

describe("knock scoring", () => {
  it("pays the knocker the deadwood difference", () => {
    // Knocker: 2-3-4♣, 7-7-7, 9-10-J♦ and a 5♥ (5). Defender: nothing melds.
    const s = settleKnock(
      cards("2c 3c 4c 7s 7h 7d 9d 10d Jd 5h"),
      cards("Ks Qh 8s 6h 2s 3h 9c Jc 4h Ad")
    );
    expect(s.outcome).toBe("knock");
    expect(s.winner).toBe("knocker");
    expect(s.knocker.deadwoodValue).toBe(5);
    expect(s.defender.deadwoodValue).toBe(10 + 10 + 8 + 6 + 2 + 3 + 9 + 10 + 4 + 1);
    expect(s.points).toBe(63 - 5);
  });

  it("counts the defender's lay-offs before comparing", () => {
    // The defender chains 5♣6♣ onto 2-3-4♣ and adds 8♦ below 9-10-J♦.
    const s = settleKnock(
      cards("2c 3c 4c 7s 7h 7d 9d 10d Jd 5h"),
      cards("5c 6c 8d Ks Qs 9h 9s 2h 3s Kh")
    );
    expect(s.defender.layoffs.map((l) => l.card).sort()).toEqual(cards("5c 6c 8d").sort());
    expect(s.defender.deadwoodValue).toBe(10 + 10 + 9 + 9 + 2 + 3 + 10);
    expect(s.points).toBe(53 - 5);
  });

  it("is an undercut when the defender ends at or below the knocker", () => {
    expect(knockVerdict(8, 3)).toEqual({ outcome: "undercut", winner: "defender", points: 30 });
    expect(knockVerdict(6, 6)).toEqual({ outcome: "undercut", winner: "defender", points: 25 });
    expect(knockVerdict(6, 7)).toEqual({ outcome: "knock", winner: "knocker", points: 1 });

    // A real hand: knocker holds 9 deadwood, the defender lays off down to 4.
    const s = settleKnock(
      cards("2c 3c 4c 7s 7h 7d Qd Kd Jd 9h"),
      cards("5c 6c 8h 8s 8d As Ah Ac 10d 3h")
    );
    expect(s.outcome).toBe("undercut");
    expect(s.winner).toBe("defender");
    expect(s.defender.deadwoodValue).toBe(3);
    expect(s.points).toBe(9 - 3 + 25);
  });

  it("refuses a knock above ten", () => {
    expect(() => settleKnock(cards("2c 3c 4c 7s 7h 7d Qd Kd 9h 2h"), cards("As"))).toThrow();
  });

  it("lays the knocker's melds the way that blocks the most lay-offs", () => {
    // A♥2♥3♥ + 2♠2♦ or 2♥2♠2♦ + A♥3♥: both leave 4. The defender holds 4♥5♥,
    // which only fit the heart run, so the knocker lays down the set of twos.
    const s = settleKnock(
      cards("Ah 2h 3h 2s 2d 9c 10c Jc Qc Kc"),
      cards("4h 5h 9s Js Qd Kh 6c 7d 8s 10h")
    );
    expect(s.knocker.deadwoodValue).toBe(4);
    expect(s.knocker.melds.some((m) => m.kind === "set")).toBe(true);
    expect(s.defender.layoffs).toEqual([]);
    expect(s.points).toBe(79 - 4);
  });
});

describe("gin and big gin", () => {
  it("scores gin as 25 plus the defender's deadwood, with no lay-offs allowed", () => {
    // The defender's 8♠ would extend 5-6-7♠ after a knock — not against gin.
    const s = settleKnock(
      cards("5s 6s 7s Kh Kd Kc 2d 3d 4d 5d"),
      cards("8s 9h Qh Jc 3c 4h 6d 7h 10c As")
    );
    expect(s.outcome).toBe("gin");
    expect(s.winner).toBe("knocker");
    expect(s.defender.layoffs).toEqual([]);
    expect(s.defender.deadwoodValue).toBe(8 + 9 + 10 + 10 + 3 + 4 + 6 + 7 + 10 + 1);
    expect(s.points).toBe(25 + 68);
  });

  it("scores big gin as 31 plus the defender's deadwood", () => {
    const s = settleBigGin(
      cards("As 2s 3s 4s 5d 5h 5c 9c 10c Jc Qc"),
      cards("8s 9h Qh 3c 4h 6d 7h 10h Kd 2h")
    );
    expect(s.outcome).toBe("big-gin");
    expect(s.points).toBe(31 + s.defender.deadwoodValue);
    expect(s.defender.deadwoodValue).toBe(8 + 9 + 10 + 3 + 4 + 6 + 7 + 10 + 10 + 2);
  });

  it("refuses a big gin with anything unmelded", () => {
    expect(() =>
      settleBigGin(cards("As 2s 3s 4s 5d 5h 5c 9c 10c Jc Kd"), cards("8s 9h"))
    ).toThrow();
  });
});
