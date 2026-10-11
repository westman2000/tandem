export type DiffLine = { kind: 'same' | 'add' | 'remove'; text: string };
// Bound memory for large imported documents. A replace block is still an exact
// diff when it is too large for a minimal line alignment.
export function lineDiff(before: string, after: string): DiffLine[] {
  const a = before.split('\n'), b = after.split('\n');
  let start = 0, end = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start++;
  while (end < a.length - start && end < b.length - start && a[a.length - 1 - end] === b[b.length - 1 - end]) end++;
  const left = a.slice(start, a.length - end), right = b.slice(start, b.length - end);
  const out: DiffLine[] = a.slice(0, start).map(text => ({ kind: 'same', text }));
  if (left.length * right.length > 1_000_000) {
    out.push(...left.map(text => ({ kind: 'remove' as const, text })), ...right.map(text => ({ kind: 'add' as const, text })));
  } else {
    const table = Array.from({ length: left.length + 1 }, () => new Uint32Array(right.length + 1));
    for (let i = left.length - 1; i >= 0; i--) for (let j = right.length - 1; j >= 0; j--) table[i][j] = left[i] === right[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
    let i = 0, j = 0;
    while (i < left.length || j < right.length) {
      if (i < left.length && j < right.length && left[i] === right[j]) { out.push({ kind: 'same', text: left[i++] }); j++; }
      else if (j < right.length && (i === left.length || table[i][j + 1] >= table[i + 1][j])) out.push({ kind: 'add', text: right[j++] });
      else out.push({ kind: 'remove', text: left[i++] });
    }
  }
  if (end) out.push(...a.slice(a.length - end).map(text => ({ kind: 'same' as const, text })));
  return out;
}
