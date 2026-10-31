/** Join truthy class-name fragments; a tiny stand-in for `clsx`. */
export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(" ");
}
