/** Resolve chapter start markers against the current virtual file order.
 * Files inserted before the next marker automatically belong to the earlier
 * chapter. Names are references; neither hashes nor fixed membership lists
 * participate in this calculation.
 */
export function chapterRanges(files: readonly string[], markers: readonly string[]): {
  ranges: { start: number; end: number }[];
  problem: "unselected" | "missing" | "duplicate" | "order" | null;
} {
  const positions = new Map<string, number[]>();
  files.forEach((file, index) => positions.set(file, [...(positions.get(file) ?? []), index]));
  const starts = markers.map((marker) => positions.get(marker)?.[0] ?? -1);
  const problem = markers.some((marker) => !marker) ? "unselected"
    : starts.some((start) => start < 0) ? "missing"
      : markers.some((marker) => positions.get(marker)!.length > 1) ? "duplicate"
        : starts.some((start, index) => index > 0 && start <= starts[index - 1]!) ? "order"
          : null;
  return { ranges: starts.map((start, index) => ({ start, end: starts[index + 1] ?? files.length })), problem };
}
