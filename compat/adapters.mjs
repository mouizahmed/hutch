// What Hutch's built-in adapters declare, read from their TOML, so these checks
// fail when an agent stops matching its adapter.
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

const dir = fileURLToPath(new URL("../internal/agents/builtin/", import.meta.url));

export async function adapter(name) {
  const text = await readFile(dir + name + ".toml", "utf8");
  const mcp = text.match(/^mcp\s*=.*$/m)?.[0] ?? "";
  const field = (key) => mcp.match(new RegExp(`fields = \\{[^}]*\\b${key} = "([^"]+)"`))?.[1];
  const credentials = [...(text.match(/^credentials\s*=\s*\[[\s\S]*?^\]|^credentials\s*=.*$/m)?.[0] ?? "")
    .matchAll(/file = "([^"]+)"(?:, keys = \[([^\]]*)\])?/g)]
    .map((m) => ({ file: m[1], keys: m[2] ? [...m[2].matchAll(/"([^"]+)"/g)].map((k) => k[1]) : [] }));
  return { headers: field("headers"), credentials };
}

// loginValues is what Hutch captures as the agent's login: each declared file,
// or only its declared keys.
export async function loginValues(a, home, codexHome) {
  let out = "";
  for (const { file, keys } of a.credentials) {
    const path = file.startsWith("~/.codex/") ? codexHome + file.slice("~/.codex".length) : home + file.slice(1);
    let data;
    try {
      data = await readFile(path, "utf8");
    } catch {
      continue;
    }
    if (keys.length === 0) out += data;
    else {
      const doc = JSON.parse(data);
      out += JSON.stringify(keys.map((k) => doc[k]));
    }
  }
  return out;
}
