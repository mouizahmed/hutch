#!/usr/bin/env node
// Exercise the native clients' actual OAuth login, file store and refresh.
// Uses only a loopback authorization/MCP server and synthetic credentials.
import { spawn, execFile } from "node:child_process";
import { createServer } from "node:http";
import { createHash, randomBytes } from "node:crypto";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { createInterface } from "node:readline";
import { promisify } from "node:util";
import assert from "node:assert/strict";
import { adapter, loginValues } from "./adapters.mjs";

const run = promisify(execFile),
  home = await mkdtemp(join(tmpdir(), "hutch-oauth-"));
const env = {
  ...process.env,
  HOME: home,
  CODEX_HOME: join(home, ".codex"),
  XDG_CONFIG_HOME: join(home, ".config"),
  BROWSER: "/bin/false",
  DBUS_SESSION_BUS_ADDRESS: "",
};
delete env.CLAUDE_CONFIG_DIR;
for (const k of Object.keys(env))
  if (
    /(?:API_KEY|AUTH_TOKEN|OAUTH_TOKEN|OAUTH_REFRESH_TOKEN|AWS_ACCESS_KEY|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN)/.test(
      k,
    )
  )
    delete env[k];
const issued = [];
const codes = new Map(),
  refreshes = new Map(),
  access = new Map();
const stats = { codes: 0, refreshes: 0, authenticated: 0, calls: 0, pkce: 0 };
let base, child;
const secret = () => randomBytes(24).toString("base64url");
const send = (res, status, value, headers = {}) =>
  res
    .writeHead(status, { "Content-Type": "application/json", ...headers })
    .end(JSON.stringify(value));
const server = createServer(async (req, res) => {
  try {
    const url = new URL(req.url, base);
    let body = "";
    for await (const chunk of req) {
      body += chunk;
      if (body.length > 1 << 20) {
        send(res, 413, {});
        return;
      }
    }
    if (url.pathname.startsWith("/.well-known/oauth-protected-resource"))
      return send(res, 200, {
        resource: base + "/mcp",
        authorization_servers: [base],
        scopes_supported: ["read", "offline_access"],
      });
    if (
      url.pathname.startsWith("/.well-known/oauth-authorization-server") ||
      url.pathname === "/.well-known/openid-configuration"
    )
      return send(res, 200, {
        issuer: base,
        authorization_endpoint: base + "/authorize",
        token_endpoint: base + "/token",
        registration_endpoint: base + "/register",
        response_types_supported: ["code"],
        grant_types_supported: ["authorization_code", "refresh_token"],
        code_challenge_methods_supported: ["S256"],
        token_endpoint_auth_methods_supported: ["none"],
      });
    if (url.pathname === "/register") {
      const d = JSON.parse(body);
      return send(res, 201, {
        ...d,
        client_id: "hutch-fixture",
        client_id_issued_at: Math.floor(Date.now() / 1000),
        token_endpoint_auth_method: "none",
      });
    }
    if (url.pathname === "/authorize") {
      const target = new URL(url.searchParams.get("redirect_uri"));
      assert.equal(target.protocol, "http:");
      assert(["127.0.0.1", "localhost", "[::1]"].includes(target.hostname));
      assert.equal(url.searchParams.get("code_challenge_method"), "S256");
      const code = secret();
      codes.set(code, url.searchParams.get("code_challenge"));
      target.searchParams.set("code", code);
      target.searchParams.set("state", url.searchParams.get("state"));
      target.searchParams.set("iss", base);
      res.writeHead(302, { Location: target.href }).end();
      return;
    }
    if (url.pathname === "/token") {
      const form = new URLSearchParams(body);
      if (form.get("grant_type") === "authorization_code") {
        const expected = codes.get(form.get("code"));
        assert(expected, "unknown code");
        assert.equal(
          createHash("sha256")
            .update(form.get("code_verifier"))
            .digest("base64url"),
          expected,
        );
        codes.delete(form.get("code"));
        stats.codes++;
        stats.pkce++;
      } else if (form.get("grant_type") === "refresh_token") {
        assert(
          refreshes.has(form.get("refresh_token")),
          "refresh token was missing or reused",
        );
        refreshes.delete(form.get("refresh_token"));
        stats.refreshes++;
      } else return send(res, 400, { error: "unsupported_grant_type" });
      const at = secret(),
        rt = secret();
      issued.push(at, rt);
      access.set(at, Date.now() + 2000);
      refreshes.set(rt, true);
      return send(res, 200, {
        access_token: at,
        refresh_token: rt,
        token_type: "Bearer",
        expires_in: 2,
        scope: "read offline_access",
      });
    }
    if (url.pathname !== "/mcp") return send(res, 404, {});
    const bearer = req.headers.authorization?.replace(/^Bearer /, "");
    if (!bearer || !access.has(bearer) || access.get(bearer) <= Date.now())
      return send(
        res,
        401,
        { error: "invalid_token" },
        {
          "WWW-Authenticate": `Bearer resource_metadata="${base}/.well-known/oauth-protected-resource/mcp"`,
        },
      );
    stats.authenticated++;
    if (req.method === "DELETE") {
      res.writeHead(204).end();
      return;
    }
    if (req.method !== "POST") {
      res.writeHead(405).end();
      return;
    }
    const request = JSON.parse(body);
    if (request.id === undefined) {
      res.writeHead(202).end();
      return;
    }
    let result;
    switch (request.method) {
      case "initialize":
        result = {
          protocolVersion: request.params.protocolVersion,
          capabilities: { tools: {} },
          serverInfo: { name: "hutch-oauth-fixture", version: "1.0.0" },
        };
        break;
      case "tools/list":
        result = {
          tools: [
            {
              name: "echo",
              description: "Synthetic OAuth echo",
              inputSchema: {
                type: "object",
                properties: { marker: { type: "string" } },
                required: ["marker"],
              },
            },
          ],
        };
        break;
      case "tools/call":
        stats.calls++;
        result = {
          content: [{ type: "text", text: request.params.arguments.marker }],
        };
        break;
      case "resources/list":
        result = { resources: [] };
        break;
      case "resources/templates/list":
        result = { resourceTemplates: [] };
        break;
      case "ping":
        result = {};
        break;
      default:
        return send(res, 200, {
          jsonrpc: "2.0",
          id: request.id,
          error: { code: -32601, message: "unknown fixture method" },
        });
    }
    send(res, 200, { jsonrpc: "2.0", id: request.id, result });
  } catch {
    send(res, 400, { error: "fixture rejected request" });
  }
});

async function login(command, args) {
  const tty = command === "claude";
  const quoted = [command, ...args]
    .map((x) => "'" + x.replaceAll("'", "'\\''") + "'")
    .join(" ");
  const proc = spawn(
    tty ? "script" : command,
    tty ? ["-q", "-e", "-c", quoted, "/dev/null"] : args,
    { env, cwd: home, stdio: ["pipe", "pipe", "pipe"] },
  );
  proc.stdin.on("error", () => {});
  let output = "",
    followed = false;
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      proc.kill();
      reject(new Error(`${command} OAuth login timed out`));
    }, 45000);
    const read = (chunk) => {
      output = (output + chunk.toString()).slice(-16000);
      const match = output.match(
        /http:\/\/127\.0\.0\.1:\d+\/authorize\?[^\s\u0000-\u0020]+/,
      );
      if (!match || followed) return;
      followed = true;
      (async () => {
        const response = await fetch(match[0], { redirect: "manual" });
        const target = response.headers.get("location");
        assert(target, "authorization did not redirect");
        try {
          await fetch(target, {
            redirect: "manual",
            signal: AbortSignal.timeout(3000),
          });
        } catch {}
        if (!proc.stdin.destroyed)
          proc.stdin.write(target + (tty ? "\r" : "\n"));
      })().catch((error) => {
        proc.kill();
        reject(error);
      });
    };
    proc.stdout.on("data", read);
    proc.stderr.on("data", read);
    proc.on("error", (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    proc.on("exit", (code) => {
      clearTimeout(timeout);
      code === 0
        ? resolve()
        : reject(
            new Error(
              `${command} OAuth login failed (${code}): ${output.replaceAll(/http:\/\/\S+/g, "[fixture URL]")}`,
            ),
          );
    });
  });
}
async function codexDiscovery() {
  const proc = spawn("codex", ["app-server", "--stdio"], {
    env,
    cwd: home,
    stdio: ["pipe", "pipe", "pipe"],
  });
  child = proc;
  const pending = new Map();
  let id = 0;
  proc.stderr.resume();
  const request = (method, params) =>
    new Promise((resolve, reject) => {
      const requestID = ++id,
        timeout = setTimeout(() => {
          pending.delete(requestID);
          reject(new Error(`Codex ${method} timed out`));
        }, 30000);
      pending.set(requestID, { resolve, reject, timeout });
      proc.stdin.write(
        JSON.stringify({ id: requestID, method, params }) + "\n",
      );
    });
  createInterface({ input: proc.stdout }).on("line", (line) => {
    const m = JSON.parse(line),
      p = pending.get(m.id);
    if (!p) return;
    clearTimeout(p.timeout);
    pending.delete(m.id);
    m.error
      ? p.reject(new Error(`Codex ${m.error.message}`))
      : p.resolve(m.result);
  });
  try {
    await request("initialize", {
      clientInfo: { name: "hutch_oauth_smoke", version: "1.0" },
      capabilities: { experimentalApi: true },
    });
    proc.stdin.write(JSON.stringify({ method: "initialized" }) + "\n");
    const status = await request("mcpServerStatus/list", {});
    assert(
      status.data.find((s) => s.name === "hutch_oauth")?.tools?.echo,
      "Codex did not discover the OAuth MCP",
    );
    const thread = await request("thread/start", {
      cwd: home,
      ephemeral: true,
      approvalPolicy: "never",
      sandbox: "danger-full-access",
    });
    const result = await request("mcpServer/tool/call", {
      threadId: thread.thread.id,
      server: "hutch_oauth",
      tool: "echo",
      arguments: { marker: "oauth-fixture-marker" },
    });
    assert(JSON.stringify(result).includes("oauth-fixture-marker"));
  } finally {
    for (const p of pending.values()) clearTimeout(p.timeout);
    proc.kill();
    child = undefined;
  }
}
// Hutch keeps native MCP OAuth in the box and captures only what an adapter
// declares as its login: an MCP token there would go to every box.
async function assertLoginsExcludeMCP() {
  for (const name of ["claude", "codex"]) {
    const values = await loginValues(await adapter(name), home, env.CODEX_HOME);
    for (const token of issued)
      assert(!values.includes(token), `${name}'s declared login holds an MCP OAuth token; update its adapter`);
  }
}
try {
  await mkdir(env.CODEX_HOME, { recursive: true });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  base = `http://127.0.0.1:${server.address().port}`;
  await writeFile(
    join(env.CODEX_HOME, "config.toml"),
    `mcp_oauth_credentials_store = "file"\n[mcp_servers.hutch_oauth]\nurl = "${base}/mcp"\nstartup_timeout_sec = 15\n`,
  );
  await writeFile(
    join(home, ".claude.json"),
    JSON.stringify({
      hasCompletedOnboarding: true,
      mcpServers: { hutch_oauth: { type: "http", url: base + "/mcp" } },
    }),
  );
  await login("codex", ["mcp", "login", "--no-browser", "hutch_oauth"]);
  await new Promise((r) => setTimeout(r, 2500));
  const beforeCodex = stats.refreshes;
  await codexDiscovery();
  assert(
    stats.refreshes > beforeCodex,
    "Codex never refreshed its expired OAuth token",
  );
  console.log(
    "Codex native OAuth: PKCE login, file credentials, expiration, rotating refresh and authenticated tool call passed.",
  );
  await login("claude", ["mcp", "login", "--no-browser", "hutch_oauth"]);
  await new Promise((r) => setTimeout(r, 2500));
  const beforeClaude = stats.refreshes;
  const { stdout } = await run("claude", ["mcp", "list"], {
    env,
    cwd: home,
    timeout: 45000,
    maxBuffer: 32768,
  });
  assert(
    stdout
      .split("\n")
      .some((l) => l.includes("hutch_oauth") && l.includes("Connected")),
    "Claude did not connect with its cached OAuth login",
  );
  assert(
    stats.refreshes > beforeClaude,
    "Claude never refreshed its expired OAuth token",
  );
  console.log(
    "Claude native OAuth: PKCE login, cached credentials, expiration and rotating refresh passed.",
  );
  await assertLoginsExcludeMCP();
  const result = { stats, completed: true };
  if (process.env.HUTCH_TEST_OAUTH_REPORT)
    await writeFile(
      process.env.HUTCH_TEST_OAUTH_REPORT,
      JSON.stringify(result, null, 2),
    );
  console.log(JSON.stringify(result));
} finally {
  if (child) child.kill();
  server.closeAllConnections();
  await new Promise((resolve) => server.close(resolve));
  await rm(home, { recursive: true, force: true });
}
