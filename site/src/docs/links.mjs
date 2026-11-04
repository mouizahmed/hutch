import path from 'node:path';
import { fileURLToPath } from 'node:url';
import Slugger from 'github-slugger';

const repo = 'https://github.com/mouizahmed/hutch';
const root = path.resolve(import.meta.dirname, '../../..');
const docsDir = path.join(root, 'docs');

// route is the site path of a file under docs/.
export function route(rel) {
  const id = rel.replace(/\.md$/, '');
  return id === 'index' ? '/docs/' : `/docs/${id}/`;
}

// resolve turns a link in a doc into where it should go on the site: other docs
// become site routes, other repository files their GitHub page.
function resolve(href, file) {
  const github = href.match(/^https:\/\/github\.com\/mouizahmed\/hutch\/blob\/main\/docs\/([^#?]+\.md)(.*)$/);
  if (github) return route(github[1]) + github[2];
  if (/^[a-z]+:|^\/\/|^#|^\//i.test(href)) return href;
  const [target, rest = ''] = href.split(/(?=[#?])/);
  const abs = path.resolve(path.dirname(file), target);
  const rel = path.relative(docsDir, abs);
  if (!rel.startsWith('..') && rel.endsWith('.md')) return route(rel) + rest;
  return `${repo}/blob/main/${path.relative(root, abs)}${rest}`;
}

// docsPlugin rewrites links in a doc and gives each heading a link to itself.
export function docsPlugin(factory) {
  const source = factory.fileURL && fileURLToPath(factory.fileURL);
  if (!source || !source.startsWith(docsDir + path.sep)) return null;
  const slugger = new Slugger();
  return {
    name: 'hutch-docs',
    element: [
      {
        filter: ['a'],
        visit(node, ctx) {
          if (typeof node.properties?.href === 'string') ctx.setProperty(node, 'href', resolve(node.properties.href, source));
        },
      },
      {
        // Ids as GitHub gives them, so its links and ours agree; set here, before
        // Astro's own ids, so the anchor can point at one.
        filter: ['h1', 'h2', 'h3', 'h4', 'h5', 'h6'],
        visit(node, ctx) {
          const id = typeof node.properties?.id === 'string' ? node.properties.id : slugger.slug(ctx.textContent(node));
          ctx.setProperty(node, 'id', id);
          if (!/^h[2-4]$/.test(node.tagName)) return;
          ctx.appendChild(node, {
            type: 'element', tagName: 'a',
            properties: { href: `#${id}`, className: ['anchor'], ariaLabel: 'Link to this section' },
            children: [{ type: 'text', value: '#' }],
          });
        },
      },
    ],
  };
}
