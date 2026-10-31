"use client";

import { useFocusOnMount } from "@/lib/hooks/useFocusOnMount";

interface RulesPanelProps {
  onClose: () => void;
}

export function RulesPanel({ onClose }: RulesPanelProps) {
  const done = useFocusOnMount<HTMLButtonElement>();
  return (
    <div
      className="gr-overlay"
      role="dialog"
      aria-modal
      aria-label="How to play"
      onKeyDown={(e) => {
        if (e.key === "Escape") onClose();
      }}
    >
      <div className="gr-dialog">
        <h2 className="gr-dialog-title">How to play</h2>
        <div className="gr-rules">
          <p>
            Each player gets <b>10 cards</b>. Build <b>melds</b>: <b>sets</b> of three or four
            of a rank, or <b>runs</b> of three or more in one suit. The ace is low only:
            A-2-3 runs, Q-K-A does not.
          </p>
          <p>
            Unmelded cards are <b>deadwood</b>: aces 1, number cards their value, J Q K 10.
            The counter under your hand always shows your best melding.
          </p>
          <p>
            <b>First turn:</b> the non-dealer may take the face-up card or pass; then the
            dealer may. If both pass, the non-dealer draws from the stock.
          </p>
          <p>
            <b>Each turn</b> draw the top of the stock or the discard pile, then discard one
            card. A card taken from the pile may not go straight back.
          </p>
          <p>
            <b>Knock</b> when a discard leaves 10 or less deadwood. The defender then lays
            off cards on your melds (chains allowed) and you score the deadwood difference.
            If the defender ends level or lower, it is an <b>undercut</b>: they score the
            difference plus 25.
          </p>
          <p>
            <b>Gin</b> (no deadwood) scores 25 plus the defender&apos;s deadwood, with no
            lay-offs. <b>Big gin</b> — all 11 cards melded straight after the draw — scores
            31 plus their deadwood. If only two stock cards remain and nobody knocks, the
            hand is a draw.
          </p>
          <p>
            The first to <b>100</b> wins the game and a 100-point bonus. Both players add
            25 per hand won, and a <b>shutout</b> doubles the winner&apos;s total.
          </p>
          <p>
            Tap a card then <b>Discard</b>, or drag it onto the pile. <b>Keys:</b> 1–0 and −
            pick cards, ← → move, <b>S</b> stock (or pass the upcard), <b>D</b> discard pile,
            <b> Enter</b> discard, <b>K</b> knock, <b>G</b> gin.
          </p>
        </div>
        <button ref={done} type="button" className="gr-primary" onClick={onClose}>
          Got it
        </button>
      </div>
    </div>
  );
}
