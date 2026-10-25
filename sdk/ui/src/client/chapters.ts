/** Resolve chapter start markers against the current virtual file order.
 * Files inserted before the next marker automatically belong to the earlier
 * chapter. Names are references; neither hashes nor fixed membership lists
 * participate in this calculation.
 */
export function chapterRanges(files: readonly string[], markers: readonly string[]): {
  ranges: { start: number; end: number }[];
  /** Input-row indices in current manifest order; labels/schedules follow their marker. */
  order: number[];
  problem: "unselected" | "missing" | "duplicate" | null;
} {
  const positions = new Map<string, number[]>();
  files.forEach((file, index) => positions.set(file, [...(positions.get(file) ?? []), index]));
  const starts = markers.map((marker) => positions.get(marker)?.[0] ?? -1);
  const problem = markers.some((marker) => !marker) ? "unselected"
    : starts.some((start) => start < 0) ? "missing"
      : new Set(markers).size !== markers.length || markers.some((marker) => positions.get(marker)!.length > 1) ? "duplicate"
        : null;
  const order = starts.map((_, index) => index).sort((a, b) => starts[a]! - starts[b]!);
  const ranges = starts.map((start) => ({ start, end: files.length }));
  order.forEach((index, position) => {
    const next = order[position + 1];
    ranges[index]!.end = next === undefined ? files.length : starts[next]!;
  });
  return { ranges, order, problem };
}
