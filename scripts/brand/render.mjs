// Render the brand PNGs, or one frame of the demo, with resvg (no browser needed).
//
//   npm install --prefix /tmp/brand @resvg/resvg-js@2.6.2 dejavu-fonts-ttf@2.37.3
//   NODE_PATH=/tmp/brand/node_modules node scripts/brand/render.mjs scripts/brand/og.svg site/assets/og.png
//   NODE_PATH=/tmp/brand/node_modules node scripts/brand/render.mjs scripts/brand/apple-touch-icon.svg site/assets/apple-touch-icon.png
//   NODE_PATH=/tmp/brand/node_modules node scripts/brand/render.mjs site/assets/demo.svg frame.png 9.5
//
// With a time t (seconds), every CSS keyframe animation of the SVG is seeked to t first, so a
// frame of the demo can be checked without a browser.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';

const require = createRequire(path.join(process.cwd(), 'noop.js'));
const { Resvg } = require('@resvg/resvg-js');
const [,, src, out, tArg, width] = process.argv;
let svg = fs.readFileSync(src, 'utf8');
if (tArg !== undefined) {
  const loop = parseFloat(svg.match(/animation:k1 ([\d.]+)s/)[1]);
  const pct = parseFloat(tArg) / loop * 100;
  const kf = {};
  for (const m of svg.matchAll(/@keyframes (k\d+)\{(.*?)\}\n/g)) {
    kf[m[1]] = [...m[2].matchAll(/([\d.]+)%\{([^}]*)\}/g)].map(x => [parseFloat(x[1]), x[2]]);
  }
  const num = (d, re) => { const m = d.match(re); return m ? m.slice(1).map(Number) : null; };
  const val = (frames) => {
    let a = frames[0], b = frames[frames.length - 1];
    for (let i = 0; i < frames.length - 1; i++) if (frames[i][0] <= pct && pct <= frames[i + 1][0]) { a = frames[i]; b = frames[i + 1]; break; }
    const f = b[0] === a[0] ? 0 : (pct - a[0]) / (b[0] - a[0]);
    const lerp = (x, y) => x + (y - x) * f;
    const attrs = [];
    const oa = num(a[1], /opacity:([\d.]+)/), ob = num(b[1], /opacity:([\d.]+)/);
    if (oa) attrs.push(`opacity="${lerp(oa[0], ob[0])}"`);
    const ta = num(a[1], /translate\(([-\d.]+)px,([-\d.]+)px\)/), tb = num(b[1], /translate\(([-\d.]+)px,([-\d.]+)px\)/);
    if (ta) attrs.push(`transform="translate(${lerp(ta[0], tb[0])} ${lerp(ta[1], tb[1])})"`);
    for (const p of ['fill', 'stroke']) { const m = a[1].match(new RegExp(p + ':(#[0-9a-f]+)')); if (m) attrs.push(`${p}="${f < 1 ? m[1] : b[1].match(new RegExp(p + ':(#[0-9a-f]+)'))[1]}"`); }
    return attrs.join(' ');
  };
  svg = svg.replace(/<style>[\s\S]*?<\/style>/, '').replace(/ class="(k\d+)"/g, (_, k) => ' ' + val(kf[k]));
}
const dir = path.join(path.dirname(require.resolve('dejavu-fonts-ttf/package.json')), 'ttf') + '/';
const fonts = fs.readdirSync(dir).map(f => dir + f);
const opts = { font: { fontFiles: fonts, loadSystemFonts: false, defaultFontFamily: 'DejaVu Sans' } };
if (width) opts.fitTo = { mode: 'width', value: parseInt(width) };
fs.writeFileSync(out, new Resvg(svg, opts).render().asPng());
