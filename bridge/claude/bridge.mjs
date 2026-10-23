// Bridge between pircd and the Claude Agent SDK. pircd starts one bridge per
// open session and talks to it with JSON lines on stdin/stdout:
//
//   pircd → bridge  {"id": "…", "type": "<command>", …}
//   bridge → pircd  {"type": "response", "id": "…", "ok": true, "data": …}
//                   {"type": "event", "ev": "<name>", …}
//
// Messages are translated into pircd's transcript shape (engine.Message, which
// mirrors pi's AgentMessage) here, so the Go side only forwards them.
//
// Usage:
//   node bridge.mjs '<config json>'        run a session (see Config below)
//   node bridge.mjs list                   print stored sessions as JSON
//   node bridge.mjs models '<config json>' print the models a session can use
import {
  query,
  listSessions,
  getSessionInfo,
  getSessionMessages,
  renameSession,
} from '@anthropic-ai/claude-agent-sdk';
import { randomUUID } from 'node:crypto';
import { existsSync, readdirSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

// transcriptPath is where Claude Code keeps a session started in cwd, or ''
// if there is no such file (it is written on the first prompt). Long paths
// are encoded differently, so other project folders are searched too.
function transcriptPath(cwd, id) {
  const projects = join(homedir(), '.claude', 'projects');
  const f = join(projects, cwd.replace(/[^a-zA-Z0-9]/g, '-'), `${id}.jsonl`);
  if (existsSync(f)) return f;
  let dirs = [];
  try {
    dirs = readdirSync(projects);
  } catch {}
  for (const d of dirs) {
    const g = join(projects, d, `${id}.jsonl`);
    if (existsSync(g)) return g;
  }
  return '';
}

// pickable leaves out Claude Code's "default" alias ("Default (recommended)"),
// which only stands for another model in the list.
function pickable(models) {
  return models.filter(m => m.value !== 'default').map(m => ({ id: m.value, name: m.displayName, provider: 'anthropic' }));
}

// sdkEnv is the environment for Claude Code. With subscription auth it uses
// the login stored by `claude /login` rather than an API key.
function sdkEnv(auth) {
  if (auth === 'env') return undefined;
  const env = { ...process.env };
  delete env.ANTHROPIC_API_KEY;
  delete env.ANTHROPIC_AUTH_TOKEN;
  return env;
}

if (process.argv[2] === 'models') {
  const c = JSON.parse(process.argv[3] ?? '{}');
  // A query whose prompt never comes: enough to ask the CLI for its models.
  const q = query({
    prompt: (async function* () { await new Promise(() => {}); })(),
    options: { cwd: c.cwd || homedir(), env: sdkEnv(c.auth), settingSources: ['user'] },
  });
  process.stdout.write(JSON.stringify(pickable(await q.supportedModels())) + '\n');
  process.exit(0);
}

if (process.argv[2] === 'list') {
  const sessions = await listSessions();
  process.stdout.write(JSON.stringify(sessions.map(s => ({
    id: s.sessionId,
    cwd: s.cwd ?? '',
    file: transcriptPath(s.cwd ?? '', s.sessionId),
    name: s.customTitle ?? '',
    firstMsg: s.firstPrompt ?? s.summary ?? '',
    created: s.createdAt ?? s.lastModified,
    modified: s.lastModified,
  }))) + '\n');
  process.exit(0);
}

/**
 * @typedef {object} Config
 * @property {'create'|'open'|'fork'} mode
 * @property {string} cwd
 * @property {string} [sessionId]   open: the session; fork: the source
 * @property {string} [name]        create: initial title
 * @property {string} [model]
 * @property {string} [effort]
 * @property {string} [permissionMode]
 * @property {'subscription'|'env'} [auth]
 */
/** @type {Config} */
const cfg = JSON.parse(process.argv[2] ?? '{}');

// ---------------------------------------------------------------------------
// Output

function write(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}
function emit(ev, fields = {}) {
  write({ type: 'event', ev, ...fields });
}

// ---------------------------------------------------------------------------
// Translation to pircd's transcript shape

const toolNames = new Map(); // tool_use id → tool name

function usageOf(u) {
  if (!u) return undefined;
  return {
    input: u.input_tokens ?? 0,
    output: u.output_tokens ?? 0,
    cacheRead: u.cache_read_input_tokens ?? 0,
    cacheWrite: u.cache_creation_input_tokens ?? 0,
  };
}

function assistantBlock(b) {
  switch (b.type) {
    case 'text':
      return { type: 'text', text: b.text };
    case 'thinking':
      return { type: 'thinking', thinking: b.thinking ?? '' };
    case 'redacted_thinking':
      return { type: 'thinking', thinking: '', redacted: true };
    case 'tool_use':
    case 'server_tool_use':
    case 'mcp_tool_use':
      toolNames.set(b.id, b.name);
      return { type: 'toolCall', id: b.id, name: b.name, arguments: b.input ?? {} };
    default:
      return null;
  }
}

function assistantMessage(m) {
  return {
    role: 'assistant',
    content: (m.content ?? []).map(assistantBlock).filter(Boolean),
    model: m.model ?? '',
    usage: usageOf(m.usage),
    stopReason: m.stop_reason ?? '',
    timestamp: Date.now(),
  };
}

function resultText(content) {
  if (typeof content === 'string') return content;
  return (content ?? [])
    .map(c => (c.type === 'text' ? c.text : c.type === 'image' ? '[image]' : ''))
    .filter(Boolean)
    .join('\n');
}

// userMessages splits an API user message into pircd messages: the text the
// user typed, and one toolResult per tool_result block.
function userMessages(m, { includeText }) {
  const out = [];
  const content = typeof m.content === 'string' ? [{ type: 'text', text: m.content }] : m.content ?? [];
  const text = [];
  for (const b of content) {
    if (b.type === 'tool_result') {
      out.push({
        role: 'toolResult',
        toolCallId: b.tool_use_id,
        toolName: toolNames.get(b.tool_use_id) ?? '',
        isError: !!b.is_error,
        content: [{ type: 'text', text: resultText(b.content) }],
        timestamp: Date.now(),
      });
    } else if (b.type === 'text' && includeText) {
      text.push({ type: 'text', text: b.text });
    } else if (b.type === 'image' && includeText) {
      text.push({ type: 'image', mimeType: b.source?.media_type ?? '' });
    }
  }
  if (text.length) out.unshift({ role: 'user', content: text, timestamp: Date.now() });
  return out;
}

// history reads the stored transcript. Claude Code stores one entry per
// assistant content block; consecutive entries of one API message are merged.
async function history() {
  if (!sessionId) return [];
  const entries = await getSessionMessages(sessionId, { dir: cfg.cwd });
  const out = [];
  let last = null; // last assistant API message id
  for (const e of entries) {
    if (e.parent_tool_use_id) continue;
    const m = e.message;
    if (!m) continue;
    if (e.type === 'assistant') {
      const msg = assistantMessage(m);
      const prev = out[out.length - 1];
      if (last && m.id === last && prev?.role === 'assistant') {
        prev.content.push(...msg.content);
        prev.usage = msg.usage ?? prev.usage;
        prev.stopReason = msg.stopReason || prev.stopReason;
      } else {
        out.push(msg);
      }
      last = m.id;
    } else if (e.type === 'user') {
      out.push(...userMessages(m, { includeText: true }));
      last = null;
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Session

let sessionId = cfg.sessionId;
let name = cfg.name ?? '';
let model = cfg.model || 'default';
let effort = cfg.effort ?? '';
let lastResult = null;
let account = ''; // who the session runs as, e.g. "me@example.com (Claude Pro)"
let models = null; // ModelInfo[], cached

const options = {
  cwd: cfg.cwd,
  includePartialMessages: true,
  settingSources: ['user', 'project', 'local'],
  systemPrompt: { type: 'preset', preset: 'claude_code' },
  thinking: { type: 'adaptive', display: 'summarized' },
  permissionMode: cfg.permissionMode || 'bypassPermissions',
  allowDangerouslySkipPermissions: (cfg.permissionMode || 'bypassPermissions') === 'bypassPermissions',
  canUseTool,
  stderr: data => process.stderr.write(data),
};
if (cfg.model) options.model = cfg.model;
if (cfg.effort) options.effort = cfg.effort;
if (cfg.auth !== 'env') options.env = sdkEnv(cfg.auth);
switch (cfg.mode) {
  case 'open':
    options.resume = cfg.sessionId;
    break;
  case 'fork':
    options.resume = cfg.sessionId;
    options.forkSession = true;
    sessionId = randomUUID();
    options.sessionId = sessionId;
    break;
  default:
    sessionId = randomUUID();
    options.sessionId = sessionId;
    if (name) options.title = name;
}

if (cfg.mode === 'open') {
  const info = await getSessionInfo(cfg.sessionId, { dir: cfg.cwd }).catch(() => undefined);
  name = info?.customTitle ?? '';
}

// Prompts are fed to the SDK as an async iterable that never ends while the
// session is open.
const inbox = [];
let wake = null;
let closed = false;
async function* input() {
  for (;;) {
    while (inbox.length) yield inbox.shift();
    if (closed) return;
    await new Promise(r => (wake = r));
    wake = null;
  }
}
function send(text) {
  const uuid = randomUUID();
  inbox.push({ type: 'user', message: { role: 'user', content: text }, parent_tool_use_id: null, session_id: sessionId, uuid });
  wake?.();
  return uuid;
}

const q = query({ prompt: input(), options });

// ---------------------------------------------------------------------------
// Runs: a run lasts from a prompt until every prompt sent has been answered.

let outstanding = new Set(); // uuids of prompts not yet answered by a result
let running = false;

function startRun(uuid) {
  outstanding.add(uuid);
  if (!running) {
    running = true;
    emit('run_start');
  }
  emit('queue', { n: Math.max(0, outstanding.size - 1) });
}

function settle(result) {
  const answered = result.user_message_uuids ?? (result.user_message_uuid ? [result.user_message_uuid] : null);
  if (answered) for (const u of answered) outstanding.delete(u);
  else outstanding.clear();
  if (outstanding.size === 0 && running) {
    running = false;
    emit('run_end');
  }
  emit('queue', { n: Math.max(0, outstanding.size - 1) });
}

// ---------------------------------------------------------------------------
// Live events

let pending = null; // assistant message being assembled: {id, msg}

function flushAssistant() {
  if (!pending) return;
  const { msg } = pending;
  pending = null;
  emit('message', { message: msg });
  for (const b of msg.content) {
    if (b.type === 'toolCall') emit('tool_start', { callId: b.id, name: b.name, args: b.arguments });
  }
}

function onAssistant(m) {
  if (pending && pending.id !== m.id) flushAssistant();
  const msg = assistantMessage(m);
  if (!pending) {
    pending = { id: m.id, msg };
    return;
  }
  pending.msg.content.push(...msg.content);
  pending.msg.usage = msg.usage ?? pending.msg.usage;
  pending.msg.stopReason = msg.stopReason || pending.msg.stopReason;
}

function onStreamEvent(e) {
  switch (e.type) {
    case 'message_start':
      flushAssistant();
      emit('assistant_start');
      break;
    case 'content_block_start': {
      const b = e.content_block;
      const kind = b.type === 'text' ? 'text' : b.type.includes('thinking') ? 'thinking' : b.type.endsWith('tool_use') ? 'tool' : null;
      if (kind) emit('block_start', { idx: e.index, kind, toolName: b.name ?? '' });
      break;
    }
    case 'content_block_delta': {
      const d = e.delta;
      const text = d.type === 'text_delta' ? d.text : d.type === 'thinking_delta' ? d.thinking : null;
      if (text) emit('block_delta', { idx: e.index, text });
      break;
    }
    case 'message_delta':
      if (pending) {
        pending.msg.usage = usageOf(e.usage) ?? pending.msg.usage;
        pending.msg.stopReason = e.delta?.stop_reason ?? pending.msg.stopReason;
      }
      break;
  }
}

async function onMessage(m) {
  if (m.parent_tool_use_id) return; // subagent traffic: its result arrives as a tool result
  switch (m.type) {
    case 'stream_event':
      onStreamEvent(m.event);
      break;
    case 'assistant':
      onAssistant(m.message);
      if (m.error) emit('notice', { kind: 'error', text: `Claude: ${m.error}` });
      break;
    case 'user':
      flushAssistant();
      if (m.isReplay || m.isSynthetic) break;
      for (const msg of userMessages(m.message, { includeText: false })) {
        emit('tool_end', { callId: msg.toolCallId });
        emit('message', { message: msg });
      }
      break;
    case 'result':
      flushAssistant();
      lastResult = m;
      if (m.subtype !== 'success') {
        const errs = (m.errors ?? []).join('\n');
        if (errs) emit('notice', { kind: 'error', text: errs });
      } else if (m.is_error && m.result) {
        emit('notice', { kind: 'error', text: m.result });
      }
      settle(m);
      break;
    case 'system':
      onSystem(m);
      break;
  }
}

async function onSystem(m) {
  switch (m.subtype) {
    case 'init':
      model = m.model || model;
      emit('state_changed');
      break;
    case 'api_retry':
      emit('retry', { attempt: m.attempt, max: m.max_retries, reason: String(m.error ?? m.error_status ?? '') });
      break;
    case 'status':
      if (m.status === 'compacting') emit('compaction_start');
      break;
    case 'compact_boundary':
      emit('compaction_end');
      emit('transcript', { messages: await history().catch(() => []) });
      break;
  }
}

// ---------------------------------------------------------------------------
// Permission prompts become pircd dialogs.

const dialogs = new Map(); // id → resolve(UIResponse)

function ask(d, signal) {
  const id = randomUUID();
  return new Promise(resolve => {
    const done = resp => {
      if (!dialogs.delete(id)) return;
      emit('dialog_closed', { id });
      resolve(resp);
    };
    dialogs.set(id, done);
    signal?.addEventListener('abort', () => done({ cancelled: true }), { once: true });
    emit('dialog', { dialog: { id, ...d } });
  });
}

function describe(input) {
  for (const k of ['command', 'file_path', 'path', 'pattern', 'url', 'query']) {
    if (typeof input?.[k] === 'string') return input[k];
  }
  return JSON.stringify(input, null, 2).slice(0, 2000);
}

const ALLOW = 'Allow';
const ALLOW_SESSION = 'Allow for this session';
const DENY = 'Deny';

async function canUseTool(toolName, input, opts) {
  if (toolName === 'AskUserQuestion') return answerQuestions(input, opts.signal);
  const choices = opts.suggestions?.length ? [ALLOW, ALLOW_SESSION, DENY] : [ALLOW, DENY];
  const resp = await ask({
    method: 'select',
    title: opts.title ?? `Allow ${toolName}?`,
    message: [opts.decisionReason, describe(input)].filter(x => typeof x === 'string' && x).join('\n\n'),
    options: choices,
  }, opts.signal);
  if (resp.value === ALLOW) return { behavior: 'allow', updatedInput: input };
  if (resp.value === ALLOW_SESSION) return { behavior: 'allow', updatedInput: input, updatedPermissions: opts.suggestions };
  return { behavior: 'deny', message: 'The user denied this tool call.' };
}

async function answerQuestions(input, signal) {
  const answers = {};
  for (const question of input.questions ?? []) {
    const resp = await ask({
      method: 'select',
      title: question.header || 'Question',
      message: question.question,
      options: question.options.map(o => o.label),
    }, signal);
    if (resp.cancelled) return { behavior: 'deny', message: 'The user declined to answer.' };
    answers[question.question] = resp.value;
  }
  return { behavior: 'allow', updatedInput: { ...input, answers } };
}

// ---------------------------------------------------------------------------
// Commands

async function modelList() {
  models ??= await q.supportedModels();
  return models;
}

// currentModel finds the list entry for the session's model. The "default"
// alias is resolved to the model it stands for, never shown as itself.
async function currentModel() {
  const list = await modelList().catch(() => []);
  let want = model;
  if (want === 'default') want = list.find(m => m.value === 'default')?.resolvedModel ?? want;
  const info = list.find(m => m.value !== 'default' && (m.value === want || m.resolvedModel === want));
  return { info, model: { id: info?.value ?? model, name: info?.displayName ?? model, provider: 'anthropic' } };
}

const commands = {
  async state() {
    return { sessionId, sessionFile: transcriptPath(cfg.cwd, sessionId), sessionName: name, thinking: effort, model: (await currentModel()).model };
  },
  messages: () => history(),
  prompt({ text }) {
    const uuid = send(text);
    emit('message', { message: { role: 'user', content: text, timestamp: Date.now() } });
    startRun(uuid);
  },
  async abort() {
    inbox.length = 0;
    await q.interrupt();
    return [];
  },
  compact({ instructions }) {
    startRun(send(('/compact ' + (instructions ?? '')).trim()));
  },
  async rename({ name: n }) {
    await renameSession(sessionId, n, { dir: cfg.cwd });
    name = n;
    emit('name', { name });
  },
  async commands() {
    return (await q.supportedCommands()).map(c => ({ name: c.name, description: c.description, source: 'claude' }));
  },
  async stats() {
    const msgs = await history().catch(() => []);
    const s = { userMessages: 0, assistantMessages: 0, toolCalls: 0, input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: lastResult?.total_cost_usd ?? 0, account };
    for (const m of msgs) {
      if (m.role === 'user') s.userMessages++;
      if (m.role === 'assistant') {
        s.assistantMessages++;
        s.toolCalls += m.content.filter(b => b.type === 'toolCall').length;
        for (const k of ['input', 'output', 'cacheRead', 'cacheWrite']) s[k] += m.usage?.[k] ?? 0;
      }
    }
    return s;
  },
  async models() {
    return pickable(await modelList());
  },
  async set_model({ model: id }) {
    await q.setModel(id === 'default' ? undefined : id);
    model = id;
  },
  async levels() {
    return (await currentModel()).info?.supportedEffortLevels ?? [];
  },
  async set_effort({ level }) {
    await q.applyFlagSettings({ effortLevel: level || null });
    effort = level;
    emit('thinking', { level });
  },
  ui({ dialogId, cancelled, confirmed, value }) {
    const done = dialogs.get(dialogId);
    if (!done) throw new Error('no such dialog');
    done(cancelled ? { cancelled: true } : { confirmed, value });
  },
};

async function handle(line) {
  let cmd;
  try {
    cmd = JSON.parse(line);
  } catch {
    return;
  }
  try {
    const fn = commands[cmd.type];
    if (!fn) throw new Error(`unknown command ${cmd.type}`);
    const data = await fn(cmd);
    write({ type: 'response', id: cmd.id, ok: true, data: data ?? null });
  } catch (err) {
    write({ type: 'response', id: cmd.id, ok: false, error: String(err?.message ?? err) });
  }
}

// stdin is split on LF only; JSON strings may contain U+2028.
let buf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', chunk => {
  buf += chunk;
  let i;
  while ((i = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, i);
    buf = buf.slice(i + 1);
    if (line.trim()) handle(line);
  }
});
process.stdin.on('end', () => {
  closed = true;
  wake?.();
  q.close?.();
  setTimeout(() => process.exit(0), 2000).unref();
});

// Note the signed-in account for the stats dialog once the CLI is up.
q.initializationResult().then(init => {
  const a = init.account ?? {};
  account = a.email ? `${a.email}${a.subscriptionType ? ` (${a.subscriptionType})` : ''}` : a.apiKeySource ? `API key (${a.apiKeySource})` : '';
}).catch(err => emit('notice', { kind: 'error', text: `Claude init failed: ${err?.message ?? err}` }));

try {
  for await (const m of q) await onMessage(m);
} catch (err) {
  process.stderr.write(`claude: ${err?.stack ?? err}\n`);
  process.exit(1);
}
process.exit(0);
