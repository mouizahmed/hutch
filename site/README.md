# Hutch website

An Astro site. The page is `src/pages/index.astro`; the constellation animation is `src/components/GlyphDither.astro`.

```sh
npm install
npm run dev      # preview at http://localhost:4321
npm run build    # static site in dist/
```

The visual reference is `brand/reference-dithered.png`.

To regenerate the shared README header, install Firefox and run:

```sh
npm run banner
```

This builds the site and writes `docs/assets/hutch-header.svg`. An optional output path and animation time can be passed to `python3 brand/make-header.py`. The generated header lives in `docs/assets/` so the site and repository share one copy.
