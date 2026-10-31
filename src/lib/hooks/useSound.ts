"use client";

import { useCallback, useRef } from "react";
import type { Voice } from "@/lib/game/session";

/** Everything the table can say: the session's voices plus the local tap tick. */
export type SoundName = Voice | "select";

/**
 * Procedural Web Audio effects — no asset files, so nothing to load or 404.
 * The context is created lazily on the first sound because browsers refuse to
 * start one before a user gesture.
 */
export function useSound(enabled: boolean) {
  const ctxRef = useRef<AudioContext | null>(null);
  const noiseRef = useRef<AudioBuffer | null>(null);

  const context = useCallback((): AudioContext | null => {
    if (typeof window === "undefined") return null;
    if (!ctxRef.current) {
      const Ctor =
        window.AudioContext ||
        (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
      if (!Ctor) return null;
      ctxRef.current = new Ctor();
    }
    if (ctxRef.current.state === "suspended") void ctxRef.current.resume();
    return ctxRef.current;
  }, []);

  const blip = useCallback(
    (
      ctx: AudioContext,
      freq: number,
      duration: number,
      type: OscillatorType,
      gain: number,
      slideTo?: number,
      delay = 0
    ) => {
      const start = ctx.currentTime + delay;
      const osc = ctx.createOscillator();
      const amp = ctx.createGain();
      osc.type = type;
      osc.frequency.setValueAtTime(freq, start);
      if (slideTo !== undefined) {
        osc.frequency.exponentialRampToValueAtTime(Math.max(slideTo, 1), start + duration);
      }
      amp.gain.setValueAtTime(gain, start);
      amp.gain.exponentialRampToValueAtTime(0.0001, start + duration);
      osc.connect(amp).connect(ctx.destination);
      osc.start(start);
      osc.stop(start + duration);
      // Release the nodes as soon as they are silent so nothing accumulates
      // over a long session.
      osc.onended = () => {
        osc.disconnect();
        amp.disconnect();
      };
    },
    []
  );

  /**
   * A short band-passed noise burst — the papery "fft" of a card sliding. The
   * buffer is filled once from a tiny LCG, so it is the same on every play.
   */
  const swish = useCallback((ctx: AudioContext, centre: number, duration: number, gain: number, delay = 0) => {
    if (!noiseRef.current) {
      const length = Math.floor(ctx.sampleRate * 0.25);
      const buffer = ctx.createBuffer(1, length, ctx.sampleRate);
      const data = buffer.getChannelData(0);
      let seed = 0x2545f491;
      for (let i = 0; i < length; i++) {
        seed = (Math.imul(seed, 1103515245) + 12345) >>> 0;
        data[i] = seed / 0x80000000 - 1;
      }
      noiseRef.current = buffer;
    }
    const start = ctx.currentTime + delay;
    const src = ctx.createBufferSource();
    src.buffer = noiseRef.current;
    const filter = ctx.createBiquadFilter();
    filter.type = "bandpass";
    filter.frequency.setValueAtTime(centre, start);
    filter.Q.value = 0.9;
    const amp = ctx.createGain();
    amp.gain.setValueAtTime(gain, start);
    amp.gain.exponentialRampToValueAtTime(0.0001, start + duration);
    src.connect(filter).connect(amp).connect(ctx.destination);
    src.start(start);
    src.stop(start + duration);
    src.onended = () => {
      src.disconnect();
      filter.disconnect();
      amp.disconnect();
    };
  }, []);

  const play = useCallback(
    (name: SoundName) => {
      if (!enabled) return;
      const ctx = context();
      if (!ctx) return;

      switch (name) {
        case "deal":
          // A quick riffle: a run of soft card slides.
          for (let i = 0; i < 7; i++) swish(ctx, 2600 + (i % 3) * 400, 0.06, 0.05, i * 0.055);
          break;
        case "draw":
          swish(ctx, 2200, 0.12, 0.07);
          blip(ctx, 520, 0.08, "sine", 0.025, 700);
          break;
        case "take":
          swish(ctx, 1800, 0.1, 0.06);
          blip(ctx, 620, 0.09, "triangle", 0.04);
          blip(ctx, 930, 0.12, "triangle", 0.035, undefined, 0.07);
          break;
        case "discard":
          swish(ctx, 3000, 0.08, 0.06);
          blip(ctx, 330, 0.1, "triangle", 0.04, 190);
          break;
        case "pass":
          blip(ctx, 300, 0.12, "sine", 0.04, 230);
          break;
        case "select":
          blip(ctx, 880, 0.035, "square", 0.015);
          break;
        case "knock":
          // Knock knock: two low thuds on the table.
          blip(ctx, 150, 0.12, "sine", 0.12, 70);
          blip(ctx, 150, 0.12, "sine", 0.12, 70, 0.17);
          break;
        case "gin":
          [523, 659, 784, 1046, 1318].forEach((f, i) =>
            blip(ctx, f, 0.2, "triangle", 0.05, undefined, i * 0.08)
          );
          break;
        case "undercut":
          blip(ctx, 392, 0.14, "sawtooth", 0.03, 262);
          blip(ctx, 784, 0.22, "triangle", 0.05, undefined, 0.16);
          break;
        case "stalemate":
          blip(ctx, 392, 0.16, "sine", 0.04);
          blip(ctx, 392, 0.2, "sine", 0.035, undefined, 0.2);
          break;
        case "win":
          [523, 659, 784, 1046, 1318, 1568].forEach((f, i) =>
            blip(ctx, f, 0.26, "triangle", 0.055, undefined, i * 0.1)
          );
          break;
        case "lose":
          [392, 330, 262, 196].forEach((f, i) => blip(ctx, f, 0.28, "sine", 0.05, undefined, i * 0.14));
          break;
        case "reject":
          blip(ctx, 190, 0.2, "sawtooth", 0.04, 96);
          break;
      }
    },
    [blip, swish, context, enabled]
  );

  return play;
}
