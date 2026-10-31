import { BOX_BONUS, GAME_BONUS, GAME_TARGET } from "./constants";
import type { HandResult, Outcome, Seat } from "./types";

/**
 * Game scoring across hands. Hand points accumulate until someone reaches the
 * target (100). The final tally then adds:
 *
 * - the **game bonus** (100) to the winner;
 * - a **line (box) bonus** of 25 per hand won, to both players;
 * - a **shutout** — the loser never scored — doubles the winner's whole total.
 */

export interface HandRecord {
  /** 1-based hand number within the game. */
  hand: number;
  outcome: Outcome;
  winner: Seat | null;
  points: number;
  /** Running totals after this hand. */
  totals: [number, number];
}

export interface MatchScore {
  target: number;
  scores: [number, number];
  handsWon: [number, number];
  history: HandRecord[];
  /** Set as soon as a player reaches the target. */
  winner: Seat | null;
}

export function newMatchScore(target = GAME_TARGET): MatchScore {
  return { target, scores: [0, 0], handsWon: [0, 0], history: [], winner: null };
}

/** Fold one finished hand into the game score. Drawn hands are recorded but score nothing. */
export function recordHand(score: MatchScore, result: HandResult): MatchScore {
  if (score.winner !== null) throw new Error("The game is already over");
  const scores: [number, number] = [score.scores[0], score.scores[1]];
  const handsWon: [number, number] = [score.handsWon[0], score.handsWon[1]];
  if (result.winner !== null) {
    scores[result.winner] += result.points;
    handsWon[result.winner] += 1;
  }
  const record: HandRecord = {
    hand: score.history.length + 1,
    outcome: result.outcome,
    winner: result.winner,
    points: result.winner === null ? 0 : result.points,
    totals: [scores[0], scores[1]],
  };
  let winner: Seat | null = null;
  if (scores[0] >= score.target) winner = 0;
  else if (scores[1] >= score.target) winner = 1;
  return { ...score, scores, handsWon, history: [...score.history, record], winner };
}

export interface FinalTally {
  winner: Seat;
  loser: Seat;
  /** Points won in hands. */
  points: [number, number];
  gameBonus: [number, number];
  boxBonus: [number, number];
  handsWon: [number, number];
  /** The loser never scored, so the winner's total is doubled. */
  shutout: boolean;
  totals: [number, number];
  /** Winner's total minus loser's total. */
  margin: number;
}

/** The end-of-game tally with every bonus applied. */
export function finalTally(score: MatchScore): FinalTally {
  const winner = score.winner;
  if (winner === null) throw new Error("The game is not over yet");
  const loser: Seat = winner === 0 ? 1 : 0;
  const gameBonus: [number, number] = [0, 0];
  gameBonus[winner] = GAME_BONUS;
  const boxBonus: [number, number] = [
    score.handsWon[0] * BOX_BONUS,
    score.handsWon[1] * BOX_BONUS,
  ];
  const shutout = score.scores[loser] === 0;
  const totals: [number, number] = [
    score.scores[0] + gameBonus[0] + boxBonus[0],
    score.scores[1] + gameBonus[1] + boxBonus[1],
  ];
  if (shutout) totals[winner] *= 2;
  return {
    winner,
    loser,
    points: [score.scores[0], score.scores[1]],
    gameBonus,
    boxBonus,
    handsWon: [score.handsWon[0], score.handsWon[1]],
    shutout,
    totals,
    margin: totals[winner] - totals[loser],
  };
}
