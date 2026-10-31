# 🃏 GIN RUMMY

A neon **Gin Rummy** — the classic two-player card game of sets, runs and deadwood — played against a CPU opponent. Draw from the stock or the discard pile, meld your cards, then knock at 10 or less or hold out for gin. Built with **Next.js 16**, **TypeScript** (strict) and **Tailwind CSS v4**; the rules engine is pure and fully tested, melding is solved exactly rather than guessed, lay-offs are optimised for the defender, and the hardest CPU counts every card it has seen without ever peeking at yours.

## ✨ Features

| | |
| --- | --- |
| 🧠 Exact melding | A memoised bitmask search finds the least deadwood every time, splitting a four of a kind or sharing a card between a set and a run when that is better |
| 🃏 Full rules | First-turn upcard offer, no taking back a picked-up card, ace-low runs, knock at 10, gin, big gin and the two-card draw |
| 🔗 Real lay-offs | The defender's best lay-offs, chains included, weighed against their own melds; none against gin |
| 🏆 Proper scoring | Knock difference, 25 for an undercut, 25 + deadwood for gin, 31 + deadwood for big gin; game to 100 with game bonus, boxes and shutout doubling |
| 🤖 Three CPUs | Easy draws blind and knocks late, Medium plays solid deadwood, Hard tracks every card, avoids feeding your melds, simulates your hand before knocking and plays for gin when ahead |
| 🙈 Fair play | The CPU decides from what it could see at a real table: its own hand, the pile and the public record of moves |
| ✨ Neon cards | CSS-only playing cards with glowing corners and centre pips, coloured meld groups and a live deadwood counter |
| 🎞️ Card motion | Draws, discards, deals and re-sorts glide between piles and hands with FLIP transitions, no animation library |
| 📋 Scoreboard | End-of-hand layouts with lay-offs, hand-by-hand history, running totals and a final tally |
| 📈 Stats | Games won and lost and best margin per difficulty, saved locally |
| ⌨️ Keyboard + touch | Tap, drag to discard or reorder, or play entirely from the keyboard |
| 🔊 Procedural audio | Web Audio card slides, knocks and fanfares, with reduced-motion support and a phone-first layout |

## 🎮 How to play

1. Pick an opponent and deal. Each player gets 10 cards; one card is turned up beside the stock.
2. On the first turn the non-dealer may take the upcard or pass, then the dealer may. If both pass, the non-dealer draws from the stock.
3. Each turn, draw from the stock or take the top discard, then discard a card (not the one you just took from the pile).
4. Build **sets** (three or four of a rank) and **runs** (three or more in a suit, ace low). Unmelded cards are **deadwood**: A = 1, number cards at face value, J Q K = 10.
5. When a discard leaves 10 or less deadwood you may **knock**; with none at all it is **gin**, and with all eleven cards melded after your draw it is **big gin**. The defender lays off what they can, and an equal or lower count is an **undercut**.
6. Hands are scored until someone reaches 100. Bonuses are added and the final tally decides the margin.

**Keys:** `1`–`0` and `-` pick cards · `←` `→` move the pick · `S` draw from the stock (or pass the upcard) · `D` take the discard · `Enter` discard · `K` knock · `G` gin · `Esc` clear the pick.

## 🛠 Tech

- **Next.js 16** (App Router) + **React 19**
- **TypeScript 5**, strict mode
- **Tailwind CSS v4** via `@tailwindcss/postcss`
- **Vitest 3** for the core-logic unit tests
- Pure, framework-free game engine (seeded deck, meld solver, lay-off optimiser, scoring, turn state machine, card-counting CPU) under `src/lib/game`

## Getting started

```bash
npm install
npm run dev      # http://localhost:3000
```

```bash
npm run build    # production build
npm test         # run the unit tests
npm run lint     # lint the project
```
