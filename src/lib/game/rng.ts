/**
 * A small seeded PRNG so every puzzle is reproducible from its seed. Uses a
 * string hash (xmur3-style) to derive a 32-bit seed and mulberry32 to generate
 * the stream — both tiny, fast and dependency-free.
 */

export interface Rng {
  /** Uniform float in [0, 1). */
  next(): number;
  /** Uniform integer in [min, max], inclusive. */
  int(min: number, max: number): number;
  /** A random element of `arr` (which must be non-empty). */
  pick<T>(arr: readonly T[]): T;
  /** Fisher–Yates shuffle, in place, returning the same array. */
  shuffle<T>(arr: T[]): T[];
  /** Index chosen with probability proportional to `weights[index]`. */
  weighted(weights: readonly number[]): number;
}

function hashSeed(str: string): number {
  let h = 1779033703 ^ str.length;
  for (let i = 0; i < str.length; i++) {
    h = Math.imul(h ^ str.charCodeAt(i), 3432918353);
    h = (h << 13) | (h >>> 19);
  }
  return (h ^ (h >>> 16)) >>> 0;
}

function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return function next() {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function makeRng(seed: number | string): Rng {
  const seedNum = typeof seed === "number" ? seed >>> 0 : hashSeed(seed);
  const rand = mulberry32(seedNum);

  const rng: Rng = {
    next: rand,
    int(min, max) {
      return min + Math.floor(rand() * (max - min + 1));
    },
    pick(arr) {
      return arr[Math.floor(rand() * arr.length)];
    },
    shuffle(arr) {
      for (let i = arr.length - 1; i > 0; i--) {
        const j = Math.floor(rand() * (i + 1));
        const tmp = arr[i];
        arr[i] = arr[j];
        arr[j] = tmp;
      }
      return arr;
    },
    weighted(weights) {
      let total = 0;
      for (const w of weights) total += w;
      let x = rand() * total;
      for (let i = 0; i < weights.length; i++) {
        x -= weights[i];
        if (x < 0) return i;
      }
      return weights.length - 1;
    },
  };
  return rng;
}

/** A non-deterministic seed for casual play (client-side only). */
export function randomSeed(): number {
  return (Math.random() * 0x100000000) >>> 0;
}
