import { test } from 'node:test';
import assert from 'node:assert/strict';
import { lineDiff } from './diff.ts';

test('diff preserves both versions, including repeated lines and Unicode', () => {
 const cases = [['a\nb\na', 'a\na\nb'], ['', 'Новая механика'], ['Строка\n', 'Строка'], ['same', 'same'], ['first\nlast', 'first\nnew\nlast']];
 let seed = 14;
 const next = () => { seed = (seed * 1664525 + 1013904223) >>> 0; return seed; };
 for (let n = 0; n < 100; n++) cases.push([Array.from({ length: next() % 20 }, () => String(next() % 5)).join('\n'), Array.from({ length: next() % 20 }, () => String(next() % 5)).join('\n')]);
 for (const [before, after] of cases) { const diff = lineDiff(before, after); assert.equal(diff.filter(x => x.kind !== 'add').map(x => x.text).join('\n'), before); assert.equal(diff.filter(x => x.kind !== 'remove').map(x => x.text).join('\n'), after); }
});
test('large document comparison remains bounded and reconstructs exact text', () => {
 const before = Array.from({ length: 1500 }, (_, n) => `old ${n}`).join('\n'), after = Array.from({ length: 1500 }, (_, n) => `new ${n}`).join('\n');
 const diff = lineDiff(before, after); assert.equal(diff.filter(x => x.kind !== 'add').map(x => x.text).join('\n'), before); assert.equal(diff.filter(x => x.kind !== 'remove').map(x => x.text).join('\n'), after);
});
