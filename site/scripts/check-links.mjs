// Checks the built site: every link to a page here goes to a page that exists,
// and every #fragment to an id on that page.
import { readFileSync, readdirSync, existsSync } from 'node:fs';
import path from 'node:path';

const dist = path.resolve(import.meta.dirname, '../dist');
const pages = new Map();
for (const f of readdirSync(dist, { recursive: true })) {
  if (!f.endsWith('.html')) continue;
  const html = readFileSync(path.join(dist, f), 'utf8');
  const route = '/' + f.replace(/index\.html$/, '').replace(/\.html$/, '/');
  pages.set(route, { html, ids: new Set([...html.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1])) });
}

const problems = [];
for (const [route, { html }] of pages) {
  for (const [, href] of html.matchAll(/\s(?:href|src)="([^"]+)"/g)) {
    if (/^(https?:|data:|mailto:)/.test(href)) continue;
    const url = new URL(href.replaceAll('&amp;', '&'), `https://site${route}`);
    const target = pages.get(url.pathname);
    if (!target) {
      if (!existsSync(path.join(dist, decodeURIComponent(url.pathname)))) problems.push(`${route}: ${href} doesn't exist`);
      continue;
    }
    const id = decodeURIComponent(url.hash.slice(1));
    if (id && !target.ids.has(id)) problems.push(`${route}: ${href} has no #${id}`);
  }
}
for (const p of problems) console.error(p);
console.log(`${pages.size} pages, ${problems.length} broken links`);
process.exit(problems.length ? 1 : 0);
