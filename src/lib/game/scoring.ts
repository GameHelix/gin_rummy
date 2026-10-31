import { BIG_GIN_BONUS, GIN_BONUS, KNOCK_LIMIT, UNDERCUT_BONUS } from "./constants";
import { bestLayoffPlan, type LayoffPlan } from "./layoff";
import { allBestArrangements, bestArrangement } from "./melds";
import type { Arrangement, Card, Layoff, SeatLayout } from "./types";

/**
 * Settling a hand once someone lays down.
 *
 * - **Knock** (deadwood 1–10): the defender lays off, then the knocker scores
 *   the difference in deadwood — unless the defender is at or below the
 *   knocker, an **undercut**, which pays the defender the difference plus 25.
 * - **Gin** (deadwood 0): no lay-offs; 25 plus the defender's deadwood.
 * - **Big gin** (all eleven cards melded): 31 plus the defender's deadwood.
 */

export interface Settlement {
  outcome: "knock" | "gin" | "big-gin" | "undercut";
  /** Who scores: the knocker, or the defender after an undercut. */
  winner: "knocker" | "defender";
  points: number;
  knocker: SeatLayout;
  defender: SeatLayout;
}

export function layoutOf(arrangement: Arrangement, layoffs: Layoff[] = []): SeatLayout {
  return {
    melds: arrangement.melds,
    deadwood: arrangement.deadwood,
    deadwoodValue: arrangement.deadwoodValue,
    layoffs,
  };
}

/** The knock-or-undercut verdict from the two final deadwood counts. */
export function knockVerdict(
  knockerDeadwood: number,
  defenderDeadwood: number
): { outcome: "knock" | "undercut"; winner: "knocker" | "defender"; points: number } {
  if (defenderDeadwood <= knockerDeadwood) {
    return {
      outcome: "undercut",
      winner: "defender",
      points: knockerDeadwood - defenderDeadwood + UNDERCUT_BONUS,
    };
  }
  return { outcome: "knock", winner: "knocker", points: defenderDeadwood - knockerDeadwood };
}

/** The knocker's net from a verdict: positive when they score, negative when undercut. */
function knockerNet(knockerDeadwood: number, defenderDeadwood: number): number {
  const v = knockVerdict(knockerDeadwood, defenderDeadwood);
  return v.winner === "knocker" ? v.points : -v.points;
}

/**
 * Settle a knock. `knocker` is the knocker's ten cards after the face-down
 * discard; `defender` is the other hand. A knock with nothing unmelded is gin.
 *
 * When the knocker's cards can be melded several equally good ways, the one
 * that leaves the defender the fewest useful lay-offs is laid down — exactly
 * what a careful player would do at the table.
 */
export function settleKnock(knocker: readonly Card[], defender: readonly Card[]): Settlement {
  const options = allBestArrangements(knocker);
  const knockerDeadwood = options[0].deadwoodValue;
  if (knockerDeadwood > KNOCK_LIMIT) {
    throw new Error(`Cannot knock with ${knockerDeadwood} deadwood`);
  }

  if (knockerDeadwood === 0) {
    const d = bestArrangement(defender);
    return {
      outcome: "gin",
      winner: "knocker",
      points: GIN_BONUS + d.deadwoodValue,
      knocker: layoutOf(options[0]),
      defender: layoutOf(d),
    };
  }

  let chosen = options[0];
  let plan: LayoffPlan = bestLayoffPlan(chosen.melds, defender);
  for (let i = 1; i < options.length; i++) {
    const p = bestLayoffPlan(options[i].melds, defender);
    if (
      knockerNet(knockerDeadwood, p.arrangement.deadwoodValue) >
      knockerNet(knockerDeadwood, plan.arrangement.deadwoodValue)
    ) {
      chosen = options[i];
      plan = p;
    }
  }

  const verdict = knockVerdict(knockerDeadwood, plan.arrangement.deadwoodValue);
  return {
    ...verdict,
    knocker: layoutOf(chosen),
    defender: layoutOf(plan.arrangement, plan.layoffs),
  };
}

/** Settle a big gin: `knocker` holds all eleven cards, every one of them melded. */
export function settleBigGin(knocker: readonly Card[], defender: readonly Card[]): Settlement {
  const k = bestArrangement(knocker);
  if (k.deadwoodValue !== 0) throw new Error("Big gin needs all eleven cards melded");
  const d = bestArrangement(defender);
  return {
    outcome: "big-gin",
    winner: "knocker",
    points: BIG_GIN_BONUS + d.deadwoodValue,
    knocker: layoutOf(k),
    defender: layoutOf(d),
  };
}
