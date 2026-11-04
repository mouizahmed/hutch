// The docs sidebar, in order. Ids are paths under docs/ without .md; guides are
// listed by hand, release notes come from their files.
export const nav: { title: string; pages: { id: string; label: string }[] }[] = [
  {
    title: 'Start here',
    pages: [
      { id: 'index', label: 'Overview' },
      { id: 'install', label: 'Install' },
      { id: 'quickstart', label: 'First box' },
      { id: 'concepts', label: 'How Hutch works' },
    ],
  },
  {
    title: 'Work with Hutch',
    pages: [
      { id: 'templates', label: 'Projects and templates' },
      { id: 'boxes', label: 'Boxes and terminals' },
      { id: 'tui', label: 'The TUI' },
      { id: 'handoff', label: 'Local and cloud' },
      { id: 'git', label: 'Review and deliver' },
      { id: 'networking', label: 'Ports, SSH, editors' },
      { id: 'agents', label: 'Agents, logins, MCPs' },
    ],
  },
  {
    title: 'Operate Hutch',
    pages: [
      { id: 'configuration', label: 'Configuration' },
      { id: 'machines', label: 'Several machines' },
      { id: 'backups', label: 'Backups and recovery' },
      { id: 'troubleshooting', label: 'Troubleshooting' },
    ],
  },
  {
    title: 'Reference',
    pages: [
      { id: 'reference/cli', label: 'CLI' },
      { id: 'reference/settings', label: 'Settings' },
      { id: 'reference/recipe', label: 'Recipe' },
      { id: 'security', label: 'Security model' },
    ],
  },
  // After the index, filled in from docs/releases/, newest first.
  { title: 'Release notes', pages: [{ id: 'changelog', label: 'All releases' }] },
  {
    title: 'Contribute',
    pages: [
      { id: 'architecture', label: 'Architecture' },
      { id: 'releases', label: 'Release preparation' },
    ],
  },
];

export const repo = 'https://github.com/mouizahmed/hutch';

// version orders release ids like releases/v0.10.0 after releases/v0.9.1.
const version = (id: string) => id.replace(/^releases\/v/, '').split('.').map(Number);

export function sidebar(ids: string[]) {
  const releases = ids
    .filter((id) => /^releases\/v\d/.test(id))
    .sort((a, b) => {
      const [x, y] = [version(a), version(b)];
      for (let i = 0; i < Math.max(x.length, y.length); i++) if ((y[i] ?? 0) !== (x[i] ?? 0)) return (y[i] ?? 0) - (x[i] ?? 0);
      return 0;
    })
    .map((id) => ({ id, label: id.slice('releases/'.length) }));
  return nav.map((g) => (g.title === 'Release notes' ? { ...g, pages: [...g.pages, ...releases] } : g));
}
