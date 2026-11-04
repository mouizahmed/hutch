# Hutch website

An Astro site: the homepage, and the guides in the repository's [`docs/`](../docs) rendered at `/docs/`. The homepage is `src/pages/index.astro`; the constellation animation is `src/components/GlyphDither.astro`.

```sh
npm install
npm run dev      # preview at http://localhost:4321
npm run build    # static site in dist/
npm run check    # build, then check every link and #fragment between pages
npm run deploy   # check, then deploy to https://usehutch.dev (Cloudflare login required)
```

## Docs

The guides stay Markdown in `docs/`, readable on GitHub; the site reads them at build time.

- The sidebar is the list in `src/docs/nav.ts`. A new guide needs an entry there; release notes in `docs/releases/` are listed on their own, newest first.
- `src/docs/links.mjs` turns links between docs into site routes (`releases/v0.2.4.md` becomes `/docs/releases/v0.2.4/`) and other repository files into their GitHub pages. It also gives headings their GitHub ids and a `#` link.
- A page's title is its `# heading`, and its description its first paragraph.

`npm run dev` caches rendered docs in `.astro/`; after changing `links.mjs`, delete `.astro/` to see the change.

## Hosting

Cloudflare Workers Static Assets serves `dist/` at `https://usehutch.dev`, configured in `wrangler.jsonc`. The site is static; it needs no server adapter or database.

The `deploy site` GitHub Actions workflow builds, checks links, and deploys pushes to `main` that change `site/`, `docs/`, or its workflow. Markdown edits therefore update the live docs automatically; adding a release note also adds it to the release navigation. New guide pages still need an entry in `src/docs/nav.ts`.

For CI, set the repository secret `CLOUDFLARE_API_TOKEN` to a Cloudflare Workers deployment token scoped to the hosting account and `usehutch.dev`. The account ID is public and is set in `wrangler.jsonc`. Local deployments use `npx wrangler login`, followed by `npm run deploy` from `site/`. Cloudflare documents the [CI token setup](https://developers.cloudflare.com/workers/ci-cd/external-cicd/github-actions/#1-authentication).

## Header artwork

The visual reference is `brand/reference-dithered.png`. To regenerate the shared README header, install Firefox and run:

```sh
npm run banner
```

This builds the site and writes `docs/assets/hutch-header.svg`. An optional output path and animation time can be passed to `python3 brand/make-header.py`. The generated header lives in `docs/assets/` so the site and repository share one copy.
