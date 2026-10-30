#!/usr/bin/env node
// Native CLI MCP smoke checks; no model turn, SDK, or provider login is used.
// Requires Node and installed Codex/Claude CLIs. Uses a temporary home only.
import { spawn, execFile } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createInterface } from 'node:readline';
import { promisify } from 'node:util';
import assert from 'node:assert/strict';

function reply(request) {
  if (request.id === undefined) return null;
  let result;
  switch (request.method) {
    case 'initialize':
      result = { protocolVersion: request.params.protocolVersion, capabilities: { tools: {} }, serverInfo: { name: 'hutch-fixture', version: '1.0.0' } };
      break;
    case 'ping': result = {}; break;
    case 'tools/list':
      result = { tools: [{ name: 'echo', description: 'Return a smoke-test marker', inputSchema: { type: 'object', properties: { marker: { type: 'string' } }, required: ['marker'] } }] };
      break;
    case 'tools/call': result = { content: [{ type: 'text', text: request.params.arguments.marker }] }; break;
    case 'resources/list': result = { resources: [] }; break;
    case 'resources/templates/list': result = { resourceTemplates: [] }; break;
    default: return { jsonrpc: '2.0', id: request.id, error: { code: -32601, message: 'Unknown method' } };
  }
  return { jsonrpc: '2.0', id: request.id, result };
}

if (process.argv[2] === 'stdio') {
  for await (const line of createInterface({ input: process.stdin })) {
    const result = reply(JSON.parse(line));
    if (result) process.stdout.write(JSON.stringify(result) + '\n');
  }
  process.exit(0);
}

const home = await mkdtemp(join(tmpdir(), 'hutch-mcp-'));
const env = { ...process.env, HOME: home, CODEX_HOME: join(home, '.codex') };
delete env.CLAUDE_CONFIG_DIR;
delete env.XDG_CONFIG_HOME;
for (const name of ['OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'CLAUDE_CODE_OAUTH_TOKEN', 'CODEX_API_KEY']) delete env[name];
const marker = 'hutch-mcp-smoke-marker';
const secret = 'hutch_synthetic_mcp_smoke_key_1234567890';
let calls = 0;
const http = createServer(async (req, res) => {
  if (req.url !== '/mcp') { res.writeHead(404).end(); return; }
  if (req.headers.authorization !== `Bearer ${secret}`) { res.writeHead(401, { 'WWW-Authenticate': 'Bearer' }).end(); return; }
  if (req.method !== 'POST') { res.writeHead(405).end(); return; }
  let data = '';
  for await (const chunk of req) data += chunk;
  const request = JSON.parse(data);
  if (request.method === 'tools/call') calls++;
  const result = reply(request);
  if (result) res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify(result));
  else res.writeHead(202).end();
});
await new Promise(resolve => http.listen(0, '127.0.0.1', resolve));
const url = `http://127.0.0.1:${http.address().port}`;
const self = fileURLToPath(import.meta.url);
const servers = {
  hutch_stdio: { command: process.execPath, args: [self, 'stdio'] },
  hutch_http: { type: 'http', url: `${url}/mcp`, headers: { Authorization: `Bearer ${secret}` } },
  hutch_bad_auth: { type: 'http', url: `${url}/mcp`, headers: { Authorization: 'Bearer wrong' } },
  hutch_bad_path: { type: 'http', url: `${url}/missing`, headers: { Authorization: `Bearer ${secret}` } },
  hutch_missing_command: { command: '/hutch-smoke-does-not-exist', args: [] },
};
const pending = new Map();
let child, stderr = '', nextID = 0;
function request(method, params) {
  const id = ++nextID;
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(() => { pending.delete(id); reject(new Error(`timeout: ${method}`)); }, 45000);
    pending.set(id, { resolve, reject, timeout });
    child.stdin.write(JSON.stringify({ id, method, params }) + '\n');
  });
}
try {
  await mkdir(env.CODEX_HOME);
  let config = '';
  for (const [name, server] of Object.entries(servers)) {
    config += `[mcp_servers.${name}]\nstartup_timeout_sec = 10\n`;
    if (server.command) config += `command = ${JSON.stringify(server.command)}\nargs = ${JSON.stringify(server.args)}\n`;
    else config += `url = ${JSON.stringify(server.url)}\nhttp_headers = { Authorization = ${JSON.stringify(server.headers.Authorization)} }\n`;
  }
  await writeFile(join(env.CODEX_HOME, 'config.toml'), config);
  child = spawn('codex', ['app-server', '--stdio'], { cwd: home, env, stdio: ['pipe', 'pipe', 'pipe'] });
  child.stderr.on('data', b => { stderr = (stderr + b).slice(-4000); });
  child.on('error', error => { for (const p of pending.values()) { clearTimeout(p.timeout); p.reject(error); } pending.clear(); });
  const lines = createInterface({ input: child.stdout });
  lines.on('line', line => {
    const message = JSON.parse(line);
    const p = pending.get(message.id);
    if (!p) return;
    clearTimeout(p.timeout);
    pending.delete(message.id);
    if (message.error) p.reject(new Error(JSON.stringify(message.error)));
    else p.resolve(message.result);
  });
  await request('initialize', { clientInfo: { name: 'hutch_mcp_smoke', version: '1.0.0' }, capabilities: { experimentalApi: true } });
  child.stdin.write(JSON.stringify({ method: 'initialized' }) + '\n');
  const status = await request('mcpServerStatus/list', {});
  const byName = new Map(status.data.map(s => [s.name, s]));
  for (const name of ['hutch_stdio', 'hutch_http']) {
    const server = byName.get(name);
    assert(server?.tools?.echo, `${name} did not discover echo: ${JSON.stringify(server)}`);
  }
  for (const name of ['hutch_bad_auth', 'hutch_bad_path', 'hutch_missing_command']) {
    assert(byName.has(name), `${name} was not evaluated`);
    assert(!byName.get(name)?.tools?.echo, `${name} unexpectedly connected`);
  }
  const thread = await request('thread/start', { cwd: home, ephemeral: true, approvalPolicy: 'never', sandbox: 'danger-full-access' });
  for (const name of ['hutch_stdio', 'hutch_http']) {
    const result = await request('mcpServer/tool/call', { threadId: thread.thread.id, server: name, tool: 'echo', arguments: { marker } });
    assert(JSON.stringify(result).includes(marker), `${name} tool returned no marker`);
  }
  assert(calls > 0, 'HTTP MCP tool was never called');
  console.log('Codex: stdio + authenticated HTTP discovery/calls passed; bad auth/path/command rejected.');

  await writeFile(join(home, '.claude.json'), JSON.stringify({ hasCompletedOnboarding: true, mcpServers: servers }));
  const { stdout } = await promisify(execFile)('claude', ['mcp', 'list'], { cwd: home, env, timeout: 90000 });
  for (const name of ['hutch_stdio', 'hutch_http']) {
    assert(stdout.split('\n').some(line => line.includes(name) && line.includes('Connected')), `${name} Claude health check failed`);
  }
  for (const name of ['hutch_bad_auth', 'hutch_bad_path', 'hutch_missing_command']) {
    assert(stdout.includes(name), `${name} Claude health check was not evaluated`);
    assert(!stdout.split('\n').some(line => line.includes(name) && line.includes('Connected')), `${name} Claude unexpectedly connected`);
  }
  console.log('Claude: stdio + authenticated HTTP health checks passed; bad auth/path/command rejected.');
} catch (error) {
  console.error(error.message.replaceAll(secret, '[synthetic token]'));
  if (stderr) console.error(stderr.replaceAll(secret, '[synthetic token]'));
  process.exitCode = 1;
} finally {
  if (child) child.kill();
  for (const p of pending.values()) clearTimeout(p.timeout);
  http.closeAllConnections();
  await new Promise(resolve => http.close(resolve));
  await rm(home, { recursive: true, force: true });
}
