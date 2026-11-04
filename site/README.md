# Hutch website

An Astro site: the homepage, and the guides in the repository's [`docs/`](../docs) rendered at `/docs/`. The homepage is `src/pages/index.astro`; the constellation animation is `src/components/GlyphDither.astro`.

```sh
npm install
npm run dev      # preview at http://localhost:4321
npm run build    # static site in dist/
npm run check    # build, then check every link and #fragment between pages
```

## Docs

The guides stay Markdown in `docs/`, readable on GitHub; the site reads them at build time.

- The sidebar is the list in `src/docs/nav.ts`. A new guide needs an entry there; release notes in `docs/releases/` are listed on their own, newest first.
- `src/docs/links.mjs` turns links between docs into site routes (`releases/v0.2.4.md` becomes `/docs/releases/v0.2.4/`) and other repository files into their GitHub pages. It also gives headings their GitHub ids and a `#` link.
- A page's title is its `# heading`, and its description its first paragraph.

`npm run dev` caches rendered docs in `.astro/`; after changing `links.mjs`, delete `.astro/` to see the change.

## Header artwork

The visual reference is `brand/reference-dithered.png`. To regenerate the shared README header, install Firefox and run:

```sh
npm run banner
```

This builds the site and writes `docs/assets/hutch-header.svg`. An optional output path and animation time can be passed to `python3 brand/make-header.py`. The generated header lives in `docs/assets/` so the site and repository share one copy.
