"use client";

import {
  useEffect,
  useEffectEvent,
  useMemo,
  useReducer,
  useRef,
  useState,
  type DragEvent,
} from "react";
import { cx } from "@/lib/cx";
import { cardLabel, cardName } from "@/lib/game/cards";
import { DIFFICULTY_LABEL, KNOCK_LIMIT, SORT_KEY } from "@/lib/game/constants";
import { chooseAction } from "@/lib/game/cpu";
import {
  discardChoices,
  lastMoveBy,
  layoutHand,
  moveCard,
  theCard,
  type SortMode,
} from "@/lib/game/display";
import { finalTally, type FinalTally, type HandRecord } from "@/lib/game/match";
import { makeRng } from "@/lib/game/rng";
import { isLegal, topDiscard, viewFor } from "@/lib/game/round";
import {
  cardKey,
  CPU,
  cpuContext,
  createSession,
  HUMAN,
  sessionReducer,
  STOCK_KEY,
} from "@/lib/game/session";
import type { Action, Card, Difficulty } from "@/lib/game/types";
import { useFlip } from "@/lib/hooks/useFlip";
import { useLocalStorage } from "@/lib/hooks/useLocalStorage";
import { useSound } from "@/lib/hooks/useSound";
import { HandResultPanel } from "./HandResultPanel";
import { Hud } from "./Hud";
import { OpponentHand } from "./OpponentHand";
import { PlayerHand, SELECT_KEYS } from "./PlayerHand";
import { CardBack, CardFace } from "./PlayingCard";
import { ScoresDialog } from "./Scoreboard";

const NAMES: readonly [string, string] = ["You", "CPU"];

/** Pauses that let the CPU's draw and its discard read as two separate moves. */
const CPU_DRAW_MS = 800;
const CPU_DISCARD_MS = 950;

const SORTS: readonly { mode: SortMode; label: string }[] = [
  { mode: "melds", label: "Melds" },
  { mode: "rank", label: "Rank" },
  { mode: "suit", label: "Suit" },
];

interface PlayScreenProps {
  difficulty: Difficulty;
  seed: number;
  soundOn: boolean;
  /** A dialog owned by the parent is open: hold the CPU and the keyboard. */
  paused: boolean;
  onGameOver: (tally: FinalTally, history: HandRecord[]) => void;
  onShowTally: () => void;
  onExit: () => void;
  onRules: () => void;
  onToggleSound: () => void;
}

/**
 * One game at the table. The session reducer holds the rules; this component
 * renders it, turns taps, drags and keys into actions, schedules the CPU's
 * moves with a visible pause, and plays the sounds the reducer asks for.
 */
export function PlayScreen({
  difficulty,
  seed,
  soundOn,
  paused,
  onGameOver,
  onShowTally,
  onExit,
  onRules,
  onToggleSound,
}: PlayScreenProps) {
  const [session, dispatch] = useReducer(
    sessionReducer,
    { difficulty, seed },
    (init: { difficulty: Difficulty; seed: number }) => createSession(init.difficulty, init.seed)
  );
  const play = useSound(soundOn);
  const sortPref = useLocalStorage<SortMode>(SORT_KEY, "melds");
  /** A hand-arranged order, valid only for the hand it was made in. */
  const [custom, setCustom] = useState<{ handNo: number; order: Card[] } | null>(null);
  const [picked, setPicked] = useState<Card | null>(null);
  const [dragging, setDragging] = useState<Card | null>(null);
  const [scoresOpen, setScoresOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  const hand = session.hand;
  const mine = hand.hands[HUMAN];
  const over = hand.phase === "over";
  const myTurn = !over && hand.turn === HUMAN;
  const cpuTurn = !over && hand.turn === CPU;
  const halted = paused || scoresOpen;
  const selected = picked !== null && mine.includes(picked) ? picked : null;
  const top = topDiscard(hand);
  const under = hand.discard.length > 1 ? hand.discard[hand.discard.length - 2] : null;
  const locked = myTurn ? hand.takenFromDiscard : null;

  const customActive = custom !== null && custom.handNo === session.handNo;
  const mode: SortMode = customActive ? "custom" : sortPref.value === "custom" ? "melds" : sortPref.value;
  const customOrder = customActive ? custom.order : null;
  const layout = useMemo(() => layoutHand(mine, mode, customOrder ?? []), [mine, mode, customOrder]);

  const discarding = myTurn && hand.phase === "discard";
  const choices = useMemo(
    () => (discarding ? discardChoices(mine, hand.takenFromDiscard) : null),
    [discarding, mine, hand.takenFromDiscard]
  );

  /* ------------------------------------------------ what the buttons do */

  const selAfter = selected !== null && choices ? choices.after.get(selected) : undefined;
  let knockCard: Card | null = null;
  if (choices) {
    if (selected !== null) {
      if (selAfter !== undefined && selAfter > 0 && selAfter <= KNOCK_LIMIT) knockCard = selected;
    } else if (choices.gin === null && choices.knock) {
      knockCard = choices.knock.card;
    }
  }
  const knockDeadwood = knockCard !== null && choices ? (choices.after.get(knockCard) ?? null) : null;
  let ginAction: Action | null = null;
  if (choices) {
    if (choices.bigGin) ginAction = { type: "big-gin" };
    else if (selected !== null) ginAction = selAfter === 0 ? { type: "knock", card: selected } : null;
    else if (choices.gin !== null) ginAction = { type: "knock", card: choices.gin };
  }
  const canDiscardSelected = choices !== null && selected !== null && selected !== hand.takenFromDiscard;
  const canStock = myTurn && hand.phase === "draw";
  const canTake = myTurn && (hand.phase === "offer" || (hand.phase === "draw" && !hand.stockOnly));
  const canDrop =
    discarding && dragging !== null && dragging !== hand.takenFromDiscard && !halted;

  const attempt = (action: Action): boolean => {
    if (!isLegal(hand, HUMAN, action)) {
      play("reject");
      return false;
    }
    dispatch({ type: "act", seat: HUMAN, action });
    return true;
  };
  const drawStock = () => attempt({ type: "draw-stock" });
  const takeDiscard = () => attempt({ type: "draw-discard" });
  const passUpcard = () => attempt({ type: "pass" });
  const discard = (card: Card) => {
    if (attempt({ type: "discard", card })) setPicked(null);
  };
  const knock = () => {
    const action: Action | null = knockCard !== null ? { type: "knock", card: knockCard } : ginAction;
    if (!action) play("reject");
    else if (attempt(action)) setPicked(null);
  };
  const gin = () => {
    if (!ginAction) play("reject");
    else if (attempt(ginAction)) setPicked(null);
  };
  const select = (card: Card) => {
    setPicked((p) => (p === card ? null : card));
    play("select");
  };
  const stockClick = () => {
    if (myTurn && hand.phase === "offer") passUpcard();
    else drawStock();
  };
  const pileClick = () => {
    if (discarding && selected !== null) discard(selected);
    else takeDiscard();
  };
  const sortBy = (m: SortMode) => {
    setCustom(null);
    sortPref.store(m);
  };
  const dropOnCard = (target: Card) => {
    if (dragging !== null && dragging !== target) {
      setCustom({ handNo: session.handNo, order: moveCard(layout.order, dragging, target) });
    }
    setDragging(null);
  };
  const dropOnPile = (e: DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    const card = dragging;
    setDragging(null);
    if (card !== null) discard(card);
  };

  /* ------------------------------------------------------------- effects */

  // The CPU moves after a pause so its draw and discard can be followed; a
  // dialog in front of the table holds it.
  useEffect(() => {
    if (halted || session.hand.phase === "over" || session.hand.turn !== CPU) return;
    const h = session.hand;
    const timer = window.setTimeout(
      () => {
        const rng = makeRng(`${session.seed}:${session.handNo}:${h.log.length}:${h.phase}`);
        const action = chooseAction(viewFor(h, CPU), session.difficulty, rng, cpuContext(session));
        dispatch({ type: "act", seat: CPU, action });
      },
      h.phase === "discard" ? CPU_DISCARD_MS : CPU_DRAW_MS
    );
    return () => window.clearTimeout(timer);
  }, [session, halted]);

  // Sound: play once per event, keyed by a monotonic nonce so toggling the
  // sound switch never replays the last effect.
  const lastVoice = useRef(0);
  useEffect(() => {
    if (session.voice && session.voiceNonce !== lastVoice.current) {
      lastVoice.current = session.voiceNonce;
      play(session.voice);
    }
  }, [session.voice, session.voiceNonce, play]);

  // Report the finished game exactly once; the parent records the stats.
  const reportGameOver = useEffectEvent(() => {
    onGameOver(finalTally(session.score), session.score.history);
  });
  const reported = useRef(false);
  const gameWinner = session.score.winner;
  useEffect(() => {
    if (gameWinner !== null && !reported.current) {
      reported.current = true;
      reportGameOver();
    }
  }, [gameWinner]);

  // Keyboard: 1–0 and − pick cards, arrows move the pick, S stock (or pass the
  // upcard), D discard pile, Enter discards, K knocks, G goes gin.
  const onKey = useEffectEvent((e: KeyboardEvent) => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    const target = e.target instanceof HTMLElement ? e.target : null;
    const k = e.key;
    const index = (SELECT_KEYS as readonly string[]).indexOf(k);
    if (index >= 0) {
      const card = layout.order[index];
      if (card !== undefined) {
        e.preventDefault();
        select(card);
      }
      return;
    }
    switch (k) {
      case "ArrowLeft":
      case "ArrowRight": {
        e.preventDefault();
        const order = layout.order;
        if (order.length === 0) return;
        const at = selected === null ? -1 : order.indexOf(selected);
        const step = k === "ArrowLeft" ? -1 : 1;
        const next = at < 0 ? (step > 0 ? 0 : order.length - 1) : (at + step + order.length) % order.length;
        setPicked(order[next]);
        play("select");
        return;
      }
      case "s":
      case "S":
        e.preventDefault();
        stockClick();
        return;
      case "d":
      case "D":
        e.preventDefault();
        takeDiscard();
        return;
      case "Enter":
        // Let ordinary focused buttons activate; a focused card means "discard it".
        if (target?.tagName === "BUTTON" && !target.classList.contains("gr-card-btn")) return;
        e.preventDefault();
        if (selected !== null) discard(selected);
        else play("reject");
        return;
      case "k":
      case "K":
        e.preventDefault();
        knock();
        return;
      case "g":
      case "G":
        e.preventDefault();
        gin();
        return;
      case "Escape":
        setPicked(null);
        return;
    }
  });
  useEffect(() => {
    if (halted || over) return;
    const handler = (e: KeyboardEvent) => onKey(e);
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [halted, over]);

  useFlip(rootRef, session.motion, `${layout.order.join(",")}|${mode}`);

  /* -------------------------------------------------------------- words */

  let prompt: string;
  let hint = "";
  if (over) prompt = "Hand over";
  else if (cpuTurn) {
    prompt =
      hand.phase === "offer" && top !== null
        ? `The CPU is weighing ${theCard(top)}…`
        : hand.phase === "discard"
          ? "The CPU is choosing a discard…"
          : "The CPU is drawing…";
  } else if (hand.phase === "offer" && top !== null) {
    prompt = `Take ${theCard(top)}?`;
    hint = hand.dealer === HUMAN ? "The CPU passed. Pass too and it draws first." : "Or pass: tap the stock.";
  } else if (hand.phase === "draw") {
    prompt = hand.stockOnly ? "Both passed: draw from the stock" : "Your draw";
    hint = hand.stockOnly ? "" : "Tap the stock or the discard pile.";
  } else if (choices?.bigGin) {
    prompt = "Big gin! All eleven cards meld";
    hint = "Press Big gin to lay them down.";
  } else if (choices?.gin != null) {
    prompt = "You can go gin!";
    hint = `Discard ${theCard(choices.gin)} with Gin.`;
  } else if (choices?.knock) {
    prompt = `You can knock with ${choices.knock.deadwood}`;
    hint = "Or discard and play on.";
  } else {
    prompt = "Discard a card";
    hint = "Tap a card, then Discard — or drag it onto the pile.";
  }

  const deadwood = choices ? (selAfter ?? choices.best) : layout.arrangement.deadwoodValue;
  const meldCount = layout.arrangement.melds.length;
  let counterNote: string;
  if (choices && selected !== null && selected === hand.takenFromDiscard) {
    counterNote = "just taken: it stays this turn";
  } else if (choices && selected !== null) {
    counterNote = `if you discard ${theCard(selected)}`;
  } else if (choices) {
    counterNote = "after your best discard";
  } else {
    counterNote = `${meldCount} meld${meldCount === 1 ? "" : "s"} · knock at ${KNOCK_LIMIT} or less`;
  }

  const lastCpuMove = lastMoveBy(hand, CPU, NAMES);
  const knockFaceDown = over && top !== null && hand.result?.knockDiscard === top;

  return (
    <div
      className="gr-table"
      ref={rootRef}
      data-phase={hand.phase}
      data-turn={over ? "none" : hand.turn === HUMAN ? "you" : "cpu"}
    >
      <Hud
        names={NAMES}
        scores={session.score.scores}
        target={session.score.target}
        dealer={hand.dealer}
        handNo={session.handNo}
        onScores={() => setScoresOpen(true)}
      />

      <OpponentHand
        count={hand.hands[CPU].length}
        label={`CPU · ${DIFFICULTY_LABEL[session.difficulty]}`}
        thinking={cpuTurn && !halted}
        lastMove={lastCpuMove}
      />

      <section className="gr-center" aria-label="Stock and discard pile">
        <button
          type="button"
          className={cx("gr-pile", "gr-stock", canStock && "is-live")}
          onClick={stockClick}
          disabled={!myTurn || hand.phase === "discard"}
          aria-label={`Stock, ${hand.stock.length} cards${canStock ? ": draw" : ""}`}
        >
          <span className="gr-slot gr-stock-top" data-flip={STOCK_KEY}>
            <CardBack />
          </span>
          <span className="gr-pile-count">{hand.stock.length}</span>
        </button>

        <div
          className={cx("gr-pile", "gr-discard", canTake && "is-live", (canDrop || canDiscardSelected) && "is-target")}
          onDragOver={(e) => {
            if (canDrop) {
              e.preventDefault();
              e.dataTransfer.dropEffect = "move";
            }
          }}
          onDrop={dropOnPile}
        >
          {under !== null ? (
            <span className="gr-slot gr-discard-under" data-flip={cardKey(under)}>
              <CardFace card={under} />
            </span>
          ) : null}
          {top !== null ? (
            <button
              type="button"
              className="gr-slot gr-discard-top"
              data-flip={cardKey(top)}
              onClick={pileClick}
              disabled={over || !(canTake || canDiscardSelected)}
              aria-label={
                canDiscardSelected && selected !== null
                  ? `Discard ${cardName(selected)}`
                  : `Discard pile, ${knockFaceDown ? "face down" : cardName(top)}${canTake ? ": take it" : ""}`
              }
            >
              {knockFaceDown ? <CardBack /> : <CardFace card={top} />}
            </button>
          ) : (
            // An empty pile is still where a discard goes, by tap as well as by drag.
            <button
              type="button"
              className="gr-discard-top gr-pile-empty"
              onClick={pileClick}
              disabled={!canDiscardSelected}
              aria-label={
                canDiscardSelected && selected !== null ? `Discard ${cardName(selected)}` : "Discard pile, empty"
              }
            >
              empty
            </button>
          )}
        </div>

        <div className="gr-prompt" aria-live="polite">
          <p className="gr-prompt-text">{prompt}</p>
          {myTurn && hand.phase === "offer" && top !== null ? (
            <div className="gr-prompt-actions">
              <button type="button" className="gr-btn is-take" onClick={takeDiscard}>
                {`Take ${cardLabel(top)}`}
              </button>
              <button type="button" className="gr-btn" onClick={passUpcard}>
                Pass
              </button>
            </div>
          ) : null}
          {hint ? <p className="gr-prompt-hint">{hint}</p> : null}
        </div>
      </section>

      <section className="gr-me" aria-label="Your hand">
        <div className="gr-me-head">
          <span
            className={cx("gr-dw", deadwood <= KNOCK_LIMIT && "is-good", deadwood === 0 && "is-gin")}
            aria-live="polite"
          >
            <span className="gr-dw-label">Deadwood</span>
            <span className="gr-dw-value">{deadwood}</span>
          </span>
          <span className="gr-dw-note">{counterNote}</span>
        </div>
        <PlayerHand
          groups={layout.groups}
          selected={selected}
          fresh={discarding ? hand.lastDrawn : null}
          locked={locked}
          draggable={!halted && !over}
          onSelect={select}
          onDragStart={setDragging}
          onDragEnd={() => setDragging(null)}
          onDropOnCard={dropOnCard}
        />
      </section>

      <div className="gr-actions">
        <button
          type="button"
          className="gr-btn"
          disabled={!canDiscardSelected}
          onClick={() => {
            if (selected !== null) discard(selected);
          }}
        >
          Discard
        </button>
        <button type="button" className="gr-btn is-knock" disabled={knockCard === null} onClick={knock}>
          {knockDeadwood !== null ? `Knock · ${knockDeadwood}` : "Knock"}
        </button>
        <button type="button" className="gr-btn is-gin" disabled={ginAction === null} onClick={gin}>
          {choices?.bigGin ? "Big gin" : "Gin"}
        </button>
      </div>

      <div className="gr-sortbar" role="group" aria-label="Sort your hand">
        <span className="gr-sortbar-label">Sort</span>
        {SORTS.map((s) => (
          <button
            key={s.mode}
            type="button"
            className={cx("gr-chip", mode === s.mode && "is-on")}
            aria-pressed={mode === s.mode}
            onClick={() => sortBy(s.mode)}
          >
            {s.label}
          </button>
        ))}
        {customActive ? <span className="gr-chip is-on is-static">Custom</span> : null}
      </div>

      <div className="gr-footer">
        <button type="button" className="gr-ghost" onClick={() => setScoresOpen(true)}>
          Scores
        </button>
        <button type="button" className="gr-ghost" onClick={onRules}>
          Rules
        </button>
        <button
          type="button"
          className="gr-ghost"
          onClick={onToggleSound}
          aria-pressed={soundOn}
          aria-label={soundOn ? "Sound on" : "Sound off"}
        >
          {soundOn ? "🔊" : "🔇"}
        </button>
        <button type="button" className="gr-ghost" onClick={onExit}>
          Menu
        </button>
      </div>

      {over && hand.result && !halted ? (
        <HandResultPanel
          result={hand.result}
          score={session.score}
          names={NAMES}
          onNext={() => {
            setPicked(null);
            dispatch({ type: "next-hand" });
          }}
          onTally={onShowTally}
          onMenu={onExit}
        />
      ) : null}

      {scoresOpen ? (
        <ScoresDialog
          history={session.score.history}
          names={NAMES}
          scores={session.score.scores}
          target={session.score.target}
          onClose={() => setScoresOpen(false)}
        />
      ) : null}
    </div>
  );
}
