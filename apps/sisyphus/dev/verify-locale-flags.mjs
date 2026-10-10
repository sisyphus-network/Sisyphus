import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import { localeOptions } from '../src/renderer/src/i18n/locales.ts';

const assets = new URL('../out/renderer/assets/', import.meta.url);
const files = await readdir(assets);
const scripts = await Promise.all(files.filter(name => name.endsWith('.js')).map(name => readFile(new URL(name, assets), 'utf8')));
const svgContents = await Promise.all(files.filter(name => name.endsWith('.svg')).map(name => readFile(new URL(name, assets), 'utf8')));
const bundled = scripts.join('\n');
// Vite's SVG data-URL encoder changes quote style and removes line breaks.
const normalize = svg => svg.trim().replaceAll('"', "'").replace(/>\s+</g, '><').replace(/[\r\n]+/g, '');
for (const [, encoded] of bundled.matchAll(/data:image\/svg\+xml,([^"\s]+)/g)) {
  svgContents.push(decodeURIComponent(encoded));
}
for (const { flag } of localeOptions) {
  const source = await readFile(new URL(`../node_modules/flagpack-core/svg/m/${flag}.svg`, import.meta.url), 'utf8');
  assert.ok(svgContents.some(svg => normalize(svg) === normalize(source)), `${flag}: package SVG missing from production renderer`);
}
assert.ok(!bundled.includes('src: `/flags/'), 'renderer must not request public-root flag URLs');
console.log(`Verified ${localeOptions.length} Flagpack SVGs in production renderer assets.`);
