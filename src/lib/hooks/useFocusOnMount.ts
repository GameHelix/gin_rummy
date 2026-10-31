"use client";

import { useEffect, useRef } from "react";

/**
 * Focus an element once it mounts, without scrolling to it. `autoFocus` would
 * scroll a dialog taller than the screen down to its button; this keeps the
 * dialog open at its top while Enter still activates the main action.
 */
export function useFocusOnMount<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  useEffect(() => {
    ref.current?.focus({ preventScroll: true });
  }, []);
  return ref;
}
