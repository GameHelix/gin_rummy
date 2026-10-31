"use client";

import { useCallback, useState } from "react";
import { cx } from "@/lib/cx";
import { DIFFICULTY_KEY, SOUND_KEY, STATS_KEY } from "@/lib/game/constants";
import type { FinalTally, HandRecord } from "@/lib/game/match";
import { randomSeed } from "@/lib/game/rng";
import { HUMAN } from "@/lib/game/session";
import type { Difficulty } from "@/lib/game/types";
import { useLocalStorage } from "@/lib/hooks/useLocalStorage";
import { MenuScreen } from "./MenuScreen";
import { PlayScreen } from "./PlayScreen";
import { ResultPanel, type TierRecord } from "./ResultPanel";
import { RulesPanel } from "./RulesPanel";

type Stats = Partial<Record<Difficulty, TierRecord>>;

const NAMES: readonly [string, string] = ["You", "CPU"];

interface ActiveGame {
  seed: number;
  difficulty: Difficulty;
  /** Bumped on every new game so PlayScreen remounts with a clean state. */
  nonce: number;
}

interface GameResult {
  tally: FinalTally;
  history: HandRecord[];
  difficulty: Difficulty;
  record: TierRecord;
  isNewBest: boolean;
  /** Recorded as soon as the game ends; shown once the player asks for the tally. */
  visible: boolean;
}

/**
 * Top-level orchestrator: routes between the menu and a game, owns the
 * persisted difficulty, sound switch and per-difficulty stats, the rules
 * dialog and the final result panel. Games are only seeded in response to a
 * click, so nothing random runs during SSR and the first paint matches the
 * server markup.
 */
export function GinRummyGame() {
  const difficulty = useLocalStorage<Difficulty>(DIFFICULTY_KEY, "medium");
  const sound = useLocalStorage<boolean>(SOUND_KEY, true);
  const stats = useLocalStorage<Stats>(STATS_KEY, {});
  const [game, setGame] = useState<ActiveGame | null>(null);
  const [rulesOpen, setRulesOpen] = useState(false);
  const [result, setResult] = useState<GameResult | null>(null);

  const start = useCallback(() => {
    const seed = randomSeed();
    const tier = difficulty.value;
    setResult(null);
    setGame((prev) => ({ seed, difficulty: tier, nonce: (prev?.nonce ?? 0) + 1 }));
  }, [difficulty.value]);

  const exit = useCallback(() => {
    setResult(null);
    setGame(null);
  }, []);

  const toggleSound = useCallback(() => sound.store(!sound.value), [sound]);

  const handleGameOver = useCallback(
    (tally: FinalTally, history: HandRecord[]) => {
      if (!game) return;
      const tier = game.difficulty;
      const prev = stats.value[tier] ?? { won: 0, lost: 0, best: 0 };
      const won = tally.winner === HUMAN;
      const record: TierRecord = {
        won: prev.won + (won ? 1 : 0),
        lost: prev.lost + (won ? 0 : 1),
        best: won ? Math.max(prev.best, tally.margin) : prev.best,
      };
      stats.store({ ...stats.value, [tier]: record });
      setResult({
        tally,
        history,
        difficulty: tier,
        record,
        isNewBest: won && tally.margin > prev.best,
        visible: false,
      });
    },
    [game, stats]
  );

  const showTally = useCallback(() => setResult((r) => (r ? { ...r, visible: true } : r)), []);

  return (
    <main className={cx("gr-app", game && "is-playing")}>
      {game ? (
        <PlayScreen
          key={game.nonce}
          difficulty={game.difficulty}
          seed={game.seed}
          soundOn={sound.value}
          paused={rulesOpen || (result?.visible ?? false)}
          onGameOver={handleGameOver}
          onShowTally={showTally}
          onExit={exit}
          onRules={() => setRulesOpen(true)}
          onToggleSound={toggleSound}
        />
      ) : (
        <MenuScreen
          difficulty={difficulty.value}
          stats={stats.value}
          soundOn={sound.value}
          onDifficulty={difficulty.store}
          onStart={start}
          onRules={() => setRulesOpen(true)}
          onToggleSound={toggleSound}
        />
      )}

      {result?.visible ? (
        <ResultPanel
          tally={result.tally}
          history={result.history}
          names={NAMES}
          difficulty={result.difficulty}
          record={result.record}
          isNewBest={result.isNewBest}
          onPlayAgain={start}
          onMenu={exit}
        />
      ) : null}

      {rulesOpen ? <RulesPanel onClose={() => setRulesOpen(false)} /> : null}
    </main>
  );
}
