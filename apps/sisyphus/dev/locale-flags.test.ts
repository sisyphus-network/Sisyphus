import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

test('every supported locale uses a bundled Flagpack SVG, not a public-root URL', async () => {
  const source = await readFile(new URL('../src/renderer/src/components/ui/locale-flag.tsx', import.meta.url), 'utf8');
  const locales = await readFile(new URL('../src/renderer/src/i18n/locales.ts', import.meta.url), 'utf8');
  for (const [, code] of locales.matchAll(/flag: "([A-Z]{2})"/g)) {
    assert.ok(source.includes(`flagpack-core/svg/m/${code}.svg?url`), code);
    const svg = await readFile(new URL(`../node_modules/flagpack-core/svg/m/${code}.svg`, import.meta.url), 'utf8');
    assert.match(svg, /<svg/);
  }
  assert.ok(!source.includes('/flags/'));
});
