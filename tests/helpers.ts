import { fullDeck, parseCard, parseCards } from "@/lib/game/cards";
import type { Card, HandState, Phase, PublicEvent, Seat } from "@/lib/game/types";

/** `cards("7s 7h 10d Qc")` — compact card lists for readable tests. */
export const cards = parseCards;
export const card = parseCard;

interface StateSpec {
  hands: [string, string];
  /** Discard pile, bottom first (the last token is the top card). */
  discard?: string;
  /** Stock, bottom first (the last token is drawn next). */
  stock?: string;
  dealer?: Seat;
  turn?: Seat;
  phase?: Phase;
  takenFromDiscard?: string;
  log?: PublicEvent[];
  /** Where the rest of the deck goes, so the table always holds all 52 cards. */
  fill?: "stock" | "discard";
}

/** Build a mid-hand state by hand. Unlisted cards are buried under the stock (or the pile). */
export function stateWith(spec: StateSpec): HandState {
  const hands: [Card[], Card[]] = [cards(spec.hands[0]), cards(spec.hands[1])];
  let discard = spec.discard ? cards(spec.discard) : [];
  let stock = spec.stock ? cards(spec.stock) : [];
  const listed = [...hands[0], ...hands[1], ...discard, ...stock];
  if (new Set(listed).size !== listed.length) throw new Error("A card is listed twice");
  const used = new Set(listed);
  const rest = fullDeck().filter((c) => !used.has(c));
  if (spec.fill === "discard") discard = [...rest, ...discard];
  else stock = [...rest, ...stock];
  return {
    hands,
    stock,
    discard,
    dealer: spec.dealer ?? 1,
    turn: spec.turn ?? 0,
    phase: spec.phase ?? "draw",
    stockOnly: false,
    takenFromDiscard: spec.takenFromDiscard ? parseCard(spec.takenFromDiscard) : null,
    lastDrawn: null,
    log: spec.log ?? [],
    result: null,
  };
}

/** True when `list` holds every card of the deck exactly once. */
export function isWholeDeck(list: readonly Card[]): boolean {
  return list.length === 52 && new Set(list).size === 52 && list.every((c) => c >= 0 && c < 52);
}
