import { defineConfig } from 'astro/config';
import { satteri } from '@astrojs/markdown-satteri';
import { docsPlugin } from './src/docs/links.mjs';

export default defineConfig({
  site: 'https://usehutch.dev',
  build: { inlineStylesheets: 'always' },
  trailingSlash: 'always',
  markdown: { processor: satteri({ hastPlugins: [docsPlugin] }) },
});
