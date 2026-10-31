"use client";

import { useCallback, useRef, useSyncExternalStore } from "react";

/**
 * localStorage-backed state.
 *
 * Written with `useSyncExternalStore` rather than "read it in an effect and
 * call setState", because that pattern costs a second render pass on every
 * mount. The store hands the server a stable snapshot (the supplied default) so
 * the first client paint matches the markup and hydration stays quiet.
 *
 * Writes in this tab notify through a module-level emitter — the browser's own
 * `storage` event only fires in *other* tabs — and that event is subscribed to
 * as well, so two open tabs stay in step.
 */
const listeners = new Map<string, Set<() => void>>();

function emit(storageKey: string) {
  listeners.get(storageKey)?.forEach((fn) => fn());
}

export function useLocalStorage<T>(storageKey: string, initial: T) {
  // Keep the fallback referentially stable even if the caller passes a literal.
  const fallback = useRef(initial);
  // Cache the parsed value so getSnapshot returns an identical reference until
  // the stored string actually changes; a fresh object every call would spin.
  const cache = useRef<{ raw: string | null; parsed: T }>({ raw: null, parsed: initial });

  const subscribe = useCallback(
    (onChange: () => void) => {
      let set = listeners.get(storageKey);
      if (!set) {
        set = new Set();
        listeners.set(storageKey, set);
      }
      set.add(onChange);

      const onStorage = (e: StorageEvent) => {
        if (e.key === storageKey) onChange();
      };
      window.addEventListener("storage", onStorage);

      return () => {
        set?.delete(onChange);
        window.removeEventListener("storage", onStorage);
      };
    },
    [storageKey]
  );

  const getSnapshot = useCallback((): T => {
    let raw: string | null = null;
    try {
      raw = window.localStorage.getItem(storageKey);
    } catch {
      // Private browsing or storage disabled: behave as if nothing is stored.
      raw = null;
    }
    if (raw !== cache.current.raw) {
      let parsed = fallback.current;
      if (raw !== null) {
        try {
          parsed = JSON.parse(raw) as T;
        } catch {
          parsed = fallback.current;
        }
      }
      cache.current = { raw, parsed };
    }
    return cache.current.parsed;
  }, [storageKey]);

  const getServerSnapshot = useCallback(() => fallback.current, []);

  const value = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);

  const store = useCallback(
    (next: T) => {
      try {
        window.localStorage.setItem(storageKey, JSON.stringify(next));
      } catch {
        // Nothing useful to do if storage is unavailable; the emit below still
        // refreshes readers for the lifetime of the page.
      }
      emit(storageKey);
    },
    [storageKey]
  );

  return { value, store };
}
