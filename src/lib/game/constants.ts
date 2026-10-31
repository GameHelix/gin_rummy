import type { Difficulty } from "./types";

/** Cards dealt to each player. */
export const HAND_SIZE = 10;

/** The most deadwood a player may hold and still knock. */
export const KNOCK_LIMIT = 10;

/**
 * The last two stock cards are never drawn: when a discard leaves the stock at
 * this size and nobody has knocked, the hand is a draw.
 */
export const STOCK_FLOOR = 2;

/** Gin: the knocker has no deadwood at all. */
export const GIN_BONUS = 25;

/** Big gin: all eleven cards melded straight after the draw. */
export const BIG_GIN_BONUS = 31;

/** Paid to a defender whose deadwood (after lay-offs) is at or below the knocker's. */
export const UNDERCUT_BONUS = 25;

/** The first player to reach this many points wins the game. */
export const GAME_TARGET = 100;

/** Added to the game winner's total. */
export const GAME_BONUS = 100;

/** The line (box) bonus, paid per hand won, to both players, when the game ends. */
export const BOX_BONUS = 25;

export const DIFFICULTIES: readonly Difficulty[] = ["easy", "medium", "hard"];

export const DIFFICULTY_LABEL: Record<Difficulty, string> = {
  easy: "Easy",
  medium: "Medium",
  hard: "Hard",
};

/** One line on what each tier does differently, shown under the picker. */
export const DIFFICULTY_BLURB: Record<Difficulty, string> = {
  easy: "Draws blind, sheds high cards and knocks late.",
  medium: "Takes only useful discards and knocks when it is safe.",
  hard: "Counts every card, avoids feeding you and weighs the undercut.",
};

/** localStorage keys. */
export const STATS_KEY = "ginrummy.stats.v1";
export const DIFFICULTY_KEY = "ginrummy.difficulty.v1";
export const SOUND_KEY = "ginrummy.sound.v1";
export const SORT_KEY = "ginrummy.sort.v1";
