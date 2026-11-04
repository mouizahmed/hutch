import { defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';

// The guides are the repository's own docs/, read at build time.
const docs = defineCollection({
  loader: glob({ pattern: '**/*.md', base: '../docs', generateId: ({ entry }) => entry.replace(/\.md$/, '') }),
});

export const collections = { docs };
