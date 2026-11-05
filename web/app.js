(() => {
  'use strict';

  // All server/user text is rendered through textContent. No HTML parsing of data.
  // Credentials live only in password inputs or the current request/setup draft.
  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
  const ACTIVE = new Set(['queued', 'running', 'waiting_approval']);
  const VIEWS = { overview: '主机概览', chat: '任务对话', approvals: '操作审批', memories: '长期记忆', timeline: '执行时间线', watch: '自动值守', channels: 'QQ 接入', settings: '工作台设置' };
  const STEPS = ['环境检查', '模型连接', '人设与记忆', '目录权限', 'QQ 接入', '自动值守', '确认完成'];
  const STATUS = {
    queued: ['排队中', 'blue'], running: ['执行中', 'green'], waiting_approval: ['待审批', 'amber'],
    completed: ['已完成', 'green'], failed: ['失败', 'red'], cancelled: ['已取消', ''], canceled: ['已取消', ''],
    interrupted: ['已中断', 'amber'], timed_out: ['已超时', 'amber'], prepared: ['参数已校验', 'blue'],
    active: ['使用中', 'green'], archived: ['已归档', ''], superseded: ['已替代', 'violet'],
    connected: ['已连接', 'green'], disconnected: ['未连接', ''], connecting: ['连接中', 'blue'],
    stopped: ['已停止', ''], waiting_callback: ['等待签名回调', 'blue'], idle: ['等待连接', ''],
    web_only: ['仅网页留存', 'blue'], reconnecting: ['正在重连', 'amber'],
    disabled: ['未启用', ''], error: ['连接异常', 'red'], dry_run: ['试运行', 'violet'],
    healthy: ['正常', 'green'], alert: ['告警', 'amber'], recovered: ['已恢复', 'green'], pending: ['等待确认', 'amber']
  };
  const state = {
    phase: 'boot', initialized: false, authenticated: false, generation: 0, view: 'overview', version: '',
    config: null, environment: {}, presets: [], secretsConfigured: {}, setup: null,
    sessions: [], tasks: [], approvals: [], timeline: [], memories: [], channels: {}, bindings: { bindings: [], pending: [] }, notifications: [],
    metrics: null, metricsAt: null, brief: '', selectedSession: '', conversation: null, conversationEpoch: 0,
    stream: null, eventID: 0, streamTimer: null, metricTimer: null, authProbe: 0, refreshSet: new Set(),
    errors: {}, loaded: {}, loading: new Map(), drafts: new Map(), snapshots: new Map(),
    memoryHits: null, memorySearchEpoch: 0, memorySearchTimer: null, sending: false, contextOpen: false
  };
  const screens = ['boot', 'auth', 'setup', 'token', 'app'];
  const wideContext = window.matchMedia('(min-width: 1600px)');
  let modal = null;
  let fieldSequence = 0;

  function text(value, max = 20000) {
    if (value === undefined || value === null) return '';
    let out = String(value);
    // Defense in depth; the backend also redacts its own secrets before emitting.
    out = out.replace(/\bBearer\s+[A-Za-z0-9._~+/=-]{8,}/gi, 'Bearer [已脱敏]')
      .replace(/\bsk-[A-Za-z0-9_-]{12,}/g, '[密钥已脱敏]')
      .replace(/((?:api[_-]?key|access[_-]?token|authorization|client[_-]?secret|password|密码|口令)\s*[=:：]\s*)(["']?)[^\s,;"'\n]{6,}\2/gi, '$1[已脱敏]')
      .replace(/((?:https?|wss?):\/\/)[^\s/@]+:[^\s/@]+@/gi, '$1[凭据已脱敏]@');
    return out.length > max ? `${out.slice(0, max)}\n… [界面已截断，最多显示 ${max.toLocaleString()} 字符]` : out;
  }
  function shortID(value, max = 10) { const clean = text(value, 256); return clean.length > max ? `${clean.slice(0, max)}…` : clean; }
  function node(tag, className, value) {
    const n = document.createElement(tag);
    if (className) n.className = className;
    if (value !== undefined) n.textContent = text(value, 65536);
    return n;
  }
  function icon(name) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.classList.add('icon'); svg.setAttribute('aria-hidden', 'true');
    const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute('href', `#i-${name}`); svg.append(use); return svg;
  }
  function button(label, className = 'secondary', iconName, handler) {
    const b = node('button', `button ${className}`); b.type = 'button';
    if (iconName) b.append(icon(iconName)); b.append(node('span', '', label));
    if (handler) b.addEventListener('click', () => runAction(handler));
    return b;
  }
  function iconButton(label, iconName, handler, className = '') {
    const b = node('button', `icon-button ${className}`); b.type = 'button'; b.title = label; b.setAttribute('aria-label', label); b.append(icon(iconName));
    b.addEventListener('click', () => runAction(handler)); return b;
  }
  function pill(label, tone = '') { return node('span', `pill ${tone}`, label); }
  function statusPill(value) { const entry = STATUS[value]; return entry ? pill(entry[0], entry[1]) : pill(value || '状态未知'); }
  function array(value, max = 100) { return Array.isArray(value) ? value.slice(0, max) : []; }
  function object(value) { return value && typeof value === 'object' && !Array.isArray(value) ? value : {}; }
  function clone(value) { return JSON.parse(JSON.stringify(value)); }
  function getAt(obj, path) { return path.split('.').reduce((a, k) => a && a[k], obj); }
  function setAt(obj, path, value) {
    const keys = path.split('.'); let target = obj;
    keys.slice(0, -1).forEach(k => { if (!target[k] || typeof target[k] !== 'object') target[k] = {}; target = target[k]; });
    target[keys[keys.length - 1]] = value;
  }
  function number(value) { return value !== null && value !== '' && value !== undefined && Number.isFinite(Number(value)) ? Number(value) : null; }
  function firstNumber(...values) { for (const value of values) { const n = number(value); if (n !== null) return n; } return null; }
  function date(value, short = false) {
    if (!value) return '时间未知'; const d = new Date(value); if (Number.isNaN(d.getTime())) return '时间未知';
    return d.toLocaleString('zh-CN', short ? { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false } : { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
  }
  function bytes(value) {
    let n = number(value); if (n === null || n < 0) return '—';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0; while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
  }
  function duration(value) {
    const n = number(value); if (n === null || n < 0) return ['—', ''];
    if (n >= 86400) return [(n / 86400).toFixed(1), '天'];
    if (n >= 3600) return [(n / 3600).toFixed(1), '小时'];
    return [Math.floor(n / 60).toString(), '分钟'];
  }
  function boundedObject(value, depth = 0) {
    if (depth > 5) return '[嵌套内容已省略]';
    if (typeof value === 'string') return text(value, 18000);
    if (Array.isArray(value)) return value.slice(0, 35).map(v => boundedObject(v, depth + 1));
    if (value && typeof value === 'object') {
      const out = {};
      Object.entries(value).slice(0, 40).forEach(([k, v]) => {
        if (/reasoning|chain_of_thought|^raw$|api.?key|secret|password|authorization|(^|_)token$/i.test(k)) return;
        out[k] = boundedObject(v, depth + 1);
      }); return out;
    }
    return value;
  }
  function stringify(value, max = 20000) { return text(typeof value === 'string' ? value : JSON.stringify(boundedObject(value), null, 2), max); }
  function eventData(event) {
    if (typeof event.data === 'string') { try { return object(JSON.parse(event.data)); } catch { return { message: event.data }; } }
    return object(event.data);
  }
  function clearSecrets(root = document) { $$('input[type="password"], input[data-secret]', root).forEach(input => { input.value = ''; }); }
  function errorMessage(error) { return text(error && error.message ? error.message : '操作未完成，请重试。', 2500); }
  function inlineError(target, error) { target.textContent = error ? errorMessage(error) : ''; target.hidden = !error; }
  function notice(message, warning = false) { const n = node('div', `notice${warning ? ' warning' : ''}`); n.append(icon(warning ? 'alert' : 'shield'), node('p', '', message)); return n; }
  function empty(title, detail, symbol = 'terminal', action) {
    const n = node('div', 'empty-state'); const mark = node('span', 'empty-icon'); mark.append(icon(symbol));
    n.append(mark, node('h3', '', title), node('p', '', detail)); if (action) n.append(action); return n;
  }
  function unavailable(key, action) { return empty('暂时无法读取', state.errors[key] || '服务正在准备数据，请稍后刷新。', 'alert', action || button('重新读取', 'secondary small', 'refresh', () => loadData(key))); }
  function toast(message, isError = false) {
    const n = node('div', `toast${isError ? ' error' : ''}`); n.append(icon(isError ? 'alert' : 'check'), node('p', '', message));
    n.append(iconButton('关闭提示', 'close', () => n.remove())); $('#toast-region').append(n);
    while ($('#toast-region').children.length > 4) $('#toast-region').firstElementChild.remove();
    window.setTimeout(() => n.remove(), isError ? 9500 : 5000);
  }
  async function runAction(action) { try { await action(); } catch (error) { if (!error.authHandled) toast(errorMessage(error), true); } }

  class APIError extends Error { constructor(message, status) { super(message); this.status = status; } }
  async function api(path, options = {}) {
    const controller = new AbortController(); const timeout = window.setTimeout(() => controller.abort(), options.timeout || 35000);
    const init = { method: options.method || 'GET', credentials: 'same-origin', cache: 'no-store', headers: { 'X-Ops-Request': '1', Accept: 'application/json' }, signal: controller.signal };
    if (options.body !== undefined) { init.body = JSON.stringify(options.body); init.headers['Content-Type'] = 'application/json'; }
    try {
      const response = await fetch(path, init);
      let data; const raw = await response.text();
      try { data = raw ? JSON.parse(raw) : {}; } catch { throw new APIError(`服务返回了非 JSON 响应（HTTP ${response.status}）。`, response.status); }
      if (!response.ok) {
        const error = new APIError(data && data.error ? text(data.error, 2500) : `请求未完成（HTTP ${response.status}）。`, response.status);
        if (response.status === 401 && !options.noAuthRedirect) { error.authHandled = true; handleUnauthorized(error.message); }
        throw error;
      }
      return data;
    } catch (error) {
      if (error.name === 'AbortError') throw new APIError('请求超时。操作结果尚不确定，请刷新核实后再决定是否重试。', 0);
      if (error instanceof APIError) throw error;
      throw new APIError('无法连接本机服务。请检查服务是否运行，以及当前访问地址。', 0);
    } finally { window.clearTimeout(timeout); init.body = undefined; }
  }

  function showScreen(name) {
    screens.forEach(id => { $(`#${id}-screen`).hidden = id !== name; });
    if (name !== 'app') { state.contextOpen = false; renderContextLayout(); }
    window.scrollTo({ top: 0, behavior: 'instant' });
  }
  function stopLive() {
    if (state.stream) state.stream.close(); state.stream = null;
    window.clearTimeout(state.streamTimer); window.clearInterval(state.metricTimer); window.clearTimeout(state.memorySearchTimer);
    state.streamTimer = null; state.metricTimer = null; state.refreshSet.clear();
  }
  function discardAuthenticatedState() {
    stopLive(); state.authenticated = false; state.generation++; state.selectedSession = ''; state.conversation = null; state.conversationEpoch++;
    state.config = null; state.environment = {}; state.secretsConfigured = {}; state.setup = null;
    state.sessions = []; state.tasks = []; state.approvals = []; state.timeline = []; state.memories = []; state.notifications = [];
    state.bindings = { bindings: [], pending: [] }; state.channels = {}; state.metrics = null; state.brief = '';
    state.loaded = {}; state.errors = {}; state.memoryHits = null; state.eventID = 0; state.drafts.clear(); state.snapshots.clear(); state.loading.clear();
    clearSecrets(); $('#new-token').value = ''; $('#message-input').value = ''; $('#memory-search').value = '';
    ['settings', 'watch', 'channels'].forEach(key => { const form = $(`#${key}-config-form`); form.replaceChildren(); delete form.dataset.loaded; delete form.dataset.dirty; });
    ['messages', 'session-list', 'recent-tasks', 'approval-list', 'memory-list', 'timeline-list', 'notifications-list', 'binding-list', 'rail-memories', 'rail-approvals', 'settings-environment'].forEach(id => $(`#${id}`).replaceChildren());
  }
  function showAuth(kind, message) {
    discardAuthenticatedState(); closeDialog(true); state.phase = kind; showScreen('auth');
    $('#login-panel').hidden = kind !== 'login'; $('#unlock-panel').hidden = kind !== 'unlock';
    inlineError($('#login-error'), null); inlineError($('#unlock-error'), null);
    if (message) inlineError($(`#${kind}-error`), new Error(message));
    requestAnimationFrame(() => $(kind === 'login' ? '#login-token' : '#setup-code').focus());
  }
  function handleUnauthorized(message) {
    if (state.phase === 'setup' || state.phase === 'unlock') showAuth('unlock', message);
    else if (state.phase !== 'login') showAuth('login', message || '登录已过期，请重新输入访问令牌。');
  }
  async function bootstrap() {
    state.phase = 'boot'; showScreen('boot'); $('#boot-retry').hidden = true; $('#boot-message').textContent = '正在检查本机服务状态…';
    try {
      const status = await api('/api/status', { noAuthRedirect: true });
      if (typeof status.initialized !== 'boolean') throw new Error('服务状态响应缺少 initialized 字段。');
      state.initialized = status.initialized; state.version = text(status.version, 60);
      if (!state.initialized) { showAuth('unlock'); return; }
      try { const config = await api('/api/config', { noAuthRedirect: true }); await enterApp(config); }
      catch (error) { if (error.status === 401) showAuth('login'); else throw error; }
    } catch (error) { $('#boot-message').textContent = errorMessage(error); $('#boot-retry').hidden = false; }
  }
  async function unlock(event) {
    event.preventDefault(); const form = event.currentTarget; if (!form.reportValidity()) return;
    const submit = $('button[type="submit"]', form); submit.disabled = true; inlineError($('#unlock-error'), null);
    let code = $('#setup-code').value.trim(); $('#setup-code').value = '';
    try {
      await api('/api/setup/unlock', { method: 'POST', body: { code }, noAuthRedirect: true }); code = '';
      const payload = await api('/api/setup/config');
      if (!payload.config) throw new Error('设置接口未返回配置。');
      state.setup = { config: clone(payload.config), secrets: {}, step: 0 };
      state.environment = object(payload.environment); state.presets = array(payload.presets, 20); state.secretsConfigured = object(payload.secrets_configured);
      state.setup.config.channels = object(state.setup.config.channels);
      for (const key of ['official', 'onebot']) { state.setup.config.channels[key] = { ...object(state.setup.config.channels[key]), enabled: false, dry_run: true }; }
      state.setup.config.watch = { ...object(state.setup.config.watch), enabled: false };
      state.phase = 'setup'; showScreen('setup'); renderSetup();
    } catch (error) { if (!error.authHandled) inlineError($('#unlock-error'), error); }
    finally { code = ''; submit.disabled = false; }
  }
  async function login(event) {
    event.preventDefault(); const form = event.currentTarget; if (!form.reportValidity()) return;
    const submit = $('button[type="submit"]', form); submit.disabled = true; inlineError($('#login-error'), null);
    let token = $('#login-token').value.trim(); $('#login-token').value = '';
    try { await api('/api/login', { method: 'POST', body: { token }, noAuthRedirect: true }); token = ''; await enterApp(); }
    catch (error) { if (!error.authHandled) inlineError($('#login-error'), error); }
    finally { token = ''; submit.disabled = false; }
  }
  function showToken(token) {
    if (typeof token !== 'string' || !token) throw new Error('服务未返回新令牌，请在服务器端核实初始化状态。');
    discardAuthenticatedState(); state.initialized = true; state.phase = 'token'; showScreen('token');
    $('#new-token').value = token; $('#new-token').type = 'password'; $('#reveal-token').textContent = '显示'; $('#token-saved').checked = false;
    $('#token-login').disabled = true; inlineError($('#token-error'), null);
  }
  async function tokenLogin() {
    if (!$('#token-saved').checked) return; const b = $('#token-login'); b.disabled = true;
    let token = $('#new-token').value;
    try { await api('/api/login', { method: 'POST', body: { token }, noAuthRedirect: true }); token = ''; $('#new-token').value = ''; await enterApp(); }
    catch (error) { inlineError($('#token-error'), error); }
    finally { token = ''; b.disabled = !$('#token-saved').checked; }
  }
  async function copyToken() {
    const input = $('#new-token'); if (!input.value) return;
    try { if (!navigator.clipboard || !window.isSecureContext) throw new Error(); await navigator.clipboard.writeText(input.value); toast('访问令牌已复制，请存入密码管理器。剪贴板不会自动清除。'); }
    catch { inlineError($('#token-error'), new Error('当前浏览器不能自动复制。请点“显示”，选中令牌并手动复制到密码管理器。')); }
  }

  function closeDialog(force = false) {
    if (modal && modal.busy && !force) return;
    const previous = modal; modal = null;
    clearSecrets($('#action-dialog')); $('#dialog-body').replaceChildren(); inlineError($('#dialog-error'), null);
    if ($('#action-dialog').open) $('#action-dialog').close();
    if (previous && previous.onCancel && !previous.submitted) previous.onCancel();
  }
  function openDialog({ title, eyebrow = 'PLEASE REVIEW', body, confirm = '确认', danger = false, wide = false, onSubmit, onCancel }) {
    if (modal && modal.busy) return;
    closeDialog(true); $('#dialog-title').textContent = title; $('#dialog-eyebrow').textContent = eyebrow;
    $('#dialog-body').replaceChildren(...(Array.isArray(body) ? body : [body])); $('#dialog-confirm').textContent = confirm;
    $('#dialog-confirm').className = `button ${danger ? 'danger' : 'primary'}`; $('#action-dialog').classList.toggle('wide', wide);
    $('#dialog-cancel').disabled = false; $('#dialog-close').disabled = false; $('#dialog-confirm').disabled = false;
    modal = { onSubmit, onCancel, busy: false, submitted: false };
    $('#action-dialog').showModal();
    requestAnimationFrame(() => { const first = $('input:not([type="checkbox"]), textarea, select', $('#dialog-body')); if (first) first.focus(); else $('#dialog-cancel').focus(); });
  }
  async function submitDialog(event) {
    event.preventDefault(); if (!modal || modal.busy || !$('#dialog-form').reportValidity()) return;
    const current = modal; current.busy = true;
    $('#dialog-confirm').disabled = true; $('#dialog-cancel').disabled = true; $('#dialog-close').disabled = true; inlineError($('#dialog-error'), null);
    try { await current.onSubmit($('#dialog-body')); current.submitted = true; if (modal === current) closeDialog(true); }
    catch (error) { if (!error.authHandled && modal === current) inlineError($('#dialog-error'), error); }
    finally { current.busy = false; if (modal === current) { $('#dialog-confirm').disabled = false; $('#dialog-cancel').disabled = false; $('#dialog-close').disabled = false; } }
  }
  function confirmAction(title, message, onSubmit, { confirm = '确认', danger = false, preview, warning } = {}) {
    const body = [node('p', '', message)];
    if (preview !== undefined) body.push(node('pre', 'output-block', stringify(preview, 65536)));
    if (warning) body.push(notice(warning, true));
    openDialog({ title, body, confirm, danger, onSubmit });
  }

  // Configuration field builders keep labels, constraints, and API names together.
  function field(label, path, value, options = {}) {
    const wrap = node('div', `field${options.wide ? ' wide' : ''}`); const id = `field-${++fieldSequence}`;
    const l = node('label', '', label); l.htmlFor = id; wrap.append(l);
    let input;
    if (options.options) {
      input = node('select'); options.options.forEach(([v, title]) => { const opt = node('option', '', title); opt.value = v; input.append(opt); });
    } else input = node(options.multiline ? 'textarea' : 'input');
    input.id = id;
    if (!options.multiline && !options.options) input.type = options.type || 'text';
    if (options.rows) input.rows = options.rows;
    if (path) input.dataset.path = path;
    if (options.secret) input.dataset.secret = options.secret;
    if (options.list) input.dataset.list = '1';
    input.value = options.list ? array(value, 32).join('\n') : value === undefined || value === null ? '' : String(value);
    if (options.required) input.required = true;
    if (options.min !== undefined) input.min = String(options.min);
    if (options.max !== undefined) input.max = String(options.max);
    if (options.step !== undefined) input.step = String(options.step);
    if (options.maxLength !== undefined) input.maxLength = options.maxLength;
    else if (!options.options && input.type !== 'number') input.maxLength = options.multiline ? 8000 : 2048;
    if (options.placeholder) input.placeholder = options.placeholder;
    if (options.disabled) input.disabled = true;
    input.autocomplete = 'off'; if (options.mono || options.secret) { input.classList.add('mono'); input.spellcheck = false; }
    if (options.secret) { input.type = 'password'; input.spellcheck = false; }
    wrap.append(input);
    if (options.help) { const help = node('small', 'field-help', options.help); help.id = `${id}-help`; input.setAttribute('aria-describedby', help.id); wrap.append(help); }
    return wrap;
  }
  function toggle(label, description, path, checked, disabled = false) {
    const wrap = node('div', 'toggle-line'); const info = node('div'); const id = `field-${++fieldSequence}`;
    const heading = node('label', 'field-label', label); heading.htmlFor = id; info.append(heading, node('p', '', description));
    const shell = node('label', 'toggle'); shell.htmlFor = id; const input = node('input'); input.type = 'checkbox'; input.id = id;
    input.checked = Boolean(checked); input.disabled = disabled; input.dataset.path = path; input.setAttribute('aria-label', label); shell.append(input, node('span'));
    wrap.append(info, shell); return wrap;
  }
  function grid(...children) { const n = node('div', 'field-grid'); n.append(...children); return n; }
  function section(title, description, symbol, ...children) {
    const wrap = node('section', 'panel form-section'); const heading = node('div', 'form-section-header'); const info = node('div');
    info.append(node('h2', '', title)); if (description) info.append(node('p', '', description));
    heading.append(icon(symbol), info); wrap.append(heading, ...children); return wrap;
  }
  function readFields(root, config) {
    $$('[data-path]', root).forEach(input => {
      if (!['INPUT', 'TEXTAREA', 'SELECT'].includes(input.tagName)) return;
      let value = input.type === 'checkbox' ? input.checked : input.type === 'number' ? Number(input.value) : input.value.trim();
      if (input.dataset.list) value = [...new Set(input.value.split(/\r?\n/).map(s => s.trim()).filter(Boolean))];
      setAt(config, input.dataset.path, value);
    }); return config;
  }
  function readSecrets(root) { const out = {}; $$('[data-secret]', root).forEach(input => { if (input.value.trim()) out[input.dataset.secret] = input.value.trim(); }); return out; }
  function secretField(label, key, isSetup, help) {
    const entered = Boolean(isSetup && state.setup && state.setup.secrets[key]); const configured = Boolean(state.secretsConfigured[key]);
    return field(label, null, '', { secret: key, maxLength: 8192, placeholder: entered ? '已在本次设置中填写；留空保留' : configured ? '服务端已保存；留空不修改' : '填写凭据（不会回显）', help: help || '仅保存在服务端。留空保留已有凭据，不能用空值删除。' });
  }
  function modelFields(config, isSetup = false) {
    const model = object(config.model);
    const body = section('模型连接', '兼容 OpenAI Chat Completions 的接口；测试只会在你确认后运行。', 'link',
      grid(field('API 地址', 'model.base_url', model.base_url, { mono: true, placeholder: 'https://api.deepseek.com', help: '远程接口必须 HTTPS；仅 localhost / 127.0.0.1 / ::1 可用 HTTP。' }), field('模型名称', 'model.name', model.name, { mono: true, maxLength: 150, placeholder: 'deepseek-chat' })),
      secretField('模型 API Key', 'model_key', isSetup));
    const actions = node('div', 'inline-actions'); actions.append(button('测试模型连接', 'secondary small', 'pulse', () => testModel(body, isSetup)), node('span', 'tiny muted', '固定测试消息，不携带主机或对话数据'));
    body.append(actions, node('div', 'test-result')); return body;
  }
  function personaFields(config, isSetup = false) {
    const p = object(config.persona); const m = object(config.memory); const presetRow = node('div', 'preset-row');
    const opts = state.presets.map(item => [item.preset, `${item.name} · ${item.preset}`]);
    if (!opts.some(([value]) => value === p.preset)) opts.unshift([p.preset || '', '当前自定义配置']);
    const preset = field('表达预设', 'persona.preset', p.preset, { options: opts.length ? opts : [['', '没有可用预设']] });
    presetRow.append(preset, button('应用预设', 'secondary small', 'refresh', () => {
      const selected = state.presets.find(item => item.preset === $('select', preset).value);
      if (!selected) { toast('没有可应用的服务端预设。', true); return; }
      confirmAction('应用表达人设预设？', '将替换名称、职责、称呼、语气和详略，保留你的个人与主机背景。保存前不会影响正在运行的配置。', () => {
        ['name', 'role', 'address', 'tone', 'detail', 'preset'].forEach(key => { const input = $(`[data-path="persona.${key}"]`, body); if (input && selected[key] !== undefined) input.value = selected[key]; });
        body.dispatchEvent(new Event('input', { bubbles: true }));
      }, { confirm: '应用预设' });
    }));
    const body = section('你的运维搭档', '人设仅影响表达，不改变权限、身份或审批规则。', 'chat', presetRow,
      grid(field('助手名称', 'persona.name', p.name, { required: true, maxLength: 32 }), field('如何称呼你', 'persona.address', p.address, { maxLength: 100 })),
      field('职责与边界', 'persona.role', p.role, { multiline: true, rows: 3, maxLength: 2000 }),
      grid(field('语气', 'persona.tone', p.tone, { maxLength: 400 }), field('回答详略', 'persona.detail', p.detail, { options: [['concise', '简洁 · 先给结论'], ['balanced', '均衡 · 结论与依据'], ['detailed', '详细 · 解释过程']] })),
      grid(field('你的背景与偏好', 'persona.owner_profile', p.owner_profile, { multiline: true, rows: 3, maxLength: 2000, help: '仅私有上下文使用；不要填写凭据。' }), field('主机背景', 'persona.host_profile', p.host_profile, { multiline: true, rows: 3, maxLength: 2000, help: '用途、服务组成等资料，不等于实时事实。' })));
    const memory = section('长期记忆', '只有显式保存或网页审批后的内容才进入长期记忆。', 'memory',
      toggle('启用记忆检索', '检索资料会附带来源与时间，可归档或删除。', 'memory.enabled', m.enabled),
      grid(field('每次最多引用条数', 'memory.max_results', m.max_results, { type: 'number', min: 1, max: 10, required: true }), field('引用字符预算', 'memory.max_chars', m.max_chars, { type: 'number', min: 100, max: 6000, required: true })));
    return [body, memory];
  }
  function permissionFields(config) {
    return section('目录与操作边界', '目录白名单由后端执行。记忆、QQ 消息和人设都不能扩大授权。', 'shield',
      field('默认工作目录', 'work_dir', config.work_dir, { required: true, mono: true, help: '服务器上的绝对路径，工具会以此目录为工作上下文。' }),
      grid(field('允许读取的目录', 'read_roots', config.read_roots, { required: true, multiline: true, rows: 4, list: true, mono: true, help: '每行一个绝对路径，1–16 个；不可授权整个文件系统根目录。' }), field('允许写入的目录', 'write_roots', config.write_roots, { multiline: true, rows: 4, list: true, mono: true, help: '每行一个绝对路径，至多 16 个；留空表示不授权目录写入。' })),
      notice('写入、命令等敏感操作仍需按具体参数在网页审批；目录授权不代表自动执行。'));
  }
  function limitFields(config) {
    const l = object(config.limits); const wrap = node('div', 'field-grid three');
    const definitions = [
      ['最大模型轮数', 'max_rounds', 1, 30, '每个任务的工具轮数上限'], ['任务超时 / 秒', 'task_seconds', 30, 1800],
      ['单命令超时 / 秒', 'command_seconds', 1, 300], ['输出上限 / 字节', 'output_bytes', 1024, 65536],
      ['上下文字符预算', 'context_chars', 4000, 120000], ['历史消息数', 'history_messages', 2, 100],
      ['记录保留 / 天', 'retention_days', 1, 365], ['最大排队任务数', 'max_queue', 1, 32]
    ];
    definitions.forEach(([label, key, min, max, help]) => wrap.append(field(label, `limits.${key}`, l[key], { type: 'number', min, max, required: true, help: help || `允许 ${min.toLocaleString()}–${max.toLocaleString()}` })));
    return section('资源预算', '为上下文、工具耗时和历史记录设置可预测的上限。', 'settings', wrap);
  }
  function channelFields(config, isSetup = false) {
    const wrap = node('div', 'channel-grid');
    for (const key of ['official', 'onebot']) {
      const c = object(object(config.channels)[key]); const title = key === 'official' ? 'QQ 官方机器人' : 'OneBot v11';
      const card = node('section', 'panel channel-card'); const header = node('div', 'channel-card-header'); const avatar = node('span', `channel-avatar ${key}`); avatar.append(icon(key === 'official' ? 'chat' : 'terminal'));
      const info = node('div'); info.append(node('h2', '', title), node('small', '', key === 'official' ? 'OFFICIAL BOT / QQ' : 'WEBSOCKET / ONEBOT'));
      const channelStatus = node('span'); channelStatus.dataset.channelStatus = key;
      channelStatus.append(isSetup ? pill('仅配置') : channelPill(key)); header.append(avatar, info, channelStatus); card.append(header);
      card.append(toggle('启用接入', isSetup ? '初始化期间不连接；完成后在网页启用。' : '保存启用配置时会连接外部服务。', `channels.${key}.enabled`, isSetup ? false : c.enabled, isSetup));
      card.append(toggle('试运行', '阻止业务消息外发；状态与预览保留在时间线。', `channels.${key}.dry_run`, isSetup ? true : c.dry_run, isSetup));
      if (key === 'official') {
        card.append(field('AppID', `channels.${key}.app_id`, c.app_id, { mono: true, maxLength: 200, help: 'QQ 开放平台的应用 ID。官方机器人身份由服务端按应用身份核验，不是普通个人 QQ 号。' }));
        const callbackURL = `${location.origin}/api/callback/qq`;
        const callback = field('签名回调地址', null, callbackURL, { mono: true, help: '由服务公开地址固定生成，不是可配置的上游 URL。生产环境必须配置可达的 HTTPS public URL，再将对应回调地址填入 QQ 开放平台。' });
        $('input', callback).readOnly = true;
        callback.append(button('复制回调地址', 'secondary small', 'copy', async () => {
          try { if (!navigator.clipboard || !window.isSecureContext) throw new Error(); await navigator.clipboard.writeText(callbackURL); toast('签名回调地址已复制。生产环境请确认是可公网到达的 HTTPS 地址。'); }
          catch { $('input', callback).focus(); $('input', callback).select(); toast('地址已选中，请手动复制。'); }
        })); card.append(callback);
        if (location.protocol !== 'https:') card.append(notice('当前为本机 HTTP 预览。此回调地址不适合生产官方接入；请先配置 HTTPS 公开地址。', true));
      } else {
        card.append(field('预期机器人 ID', `channels.${key}.bot_id`, c.bot_id, { mono: true, maxLength: 200, help: '必须与 OneBot 实例的机器人 QQ 号一致。' }));
        card.append(field('正向 WebSocket 地址', `channels.${key}.url`, c.url, { mono: true, placeholder: 'ws://127.0.0.1:3001', help: '使用 ws/wss，不要把凭据放在 URL 用户信息或查询参数中。' }));
      }
      card.append(secretField(key === 'onebot' ? 'OneBot Access Token' : '应用 Secret', key === 'onebot' ? 'onebot_token' : 'official_secret', isSetup));
      card.append(field('授权群列表', `channels.${key}.groups`, c.groups, { multiline: true, rows: 3, list: true, mono: true, help: '每行一个群 ID，最多 32 个。群聊始终只读，仅使用本群范围的资料。' }));
      if (!isSetup) card.append(button('测试已保存的连接', 'secondary small full', 'pulse', () => testChannel(key, card)), node('div', 'test-result'));
      wrap.append(card);
    }
    return wrap;
  }
  function watchFields(config, isSetup = false) {
    const w = object(config.watch);
    return section('巡检与通知策略', '默认关闭；规则基于真实采样，通知不包含日志正文或凭据。', 'pulse',
      toggle('启用自动值守', isSetup ? '初始化阶段保持关闭；完成后可在“自动值守”页显式启用。' : '只有保存后生效。未启用的渠道不会因此自动建立连接。', 'watch.enabled', isSetup ? false : w.enabled, isSetup),
      grid(field('采样间隔 / 秒', 'watch.interval_seconds', w.interval_seconds, { type: 'number', min: 30, max: 86400, required: true }), field('连续命中次数', 'watch.consecutive', w.consecutive, { type: 'number', min: 1, max: 20, required: true, help: '连续达到阈值才产生通知。' })),
      (() => { const g = node('div', 'field-grid three'); [['CPU 阈值 / %', 'cpu_percent'], ['内存阈值 / %', 'memory_percent'], ['磁盘阈值 / %', 'disk_percent']].forEach(([label, key]) => g.append(field(label, `watch.${key}`, w[key], { type: 'number', min: 1, max: 100, step: .1, required: true }))); return g; })(),
      grid(field('通知冷却 / 秒', 'watch.cooldown_seconds', w.cooldown_seconds, { type: 'number', min: 60, max: 604800, required: true, help: '同类状态避免重复打扰，建议至少 1800 秒。' }), field('每日通知额度', 'watch.daily_limit', w.daily_limit, { type: 'number', min: 1, max: 100, required: true })),
      grid(field('免打扰开始', 'watch.quiet_start', w.quiet_start, { type: 'time', help: '按服务器本地时间；留空不设置。' }), field('免打扰结束', 'watch.quiet_end', w.quiet_end, { type: 'time', help: '可跨午夜，例如 23:00 至 08:00。' })),
      grid(field('通知渠道', 'watch.notify_channel', w.notify_channel, { options: [['', '不向外发送 · 仅工作台'], ['official', 'QQ 官方机器人'], ['onebot', 'OneBot v11']] }), field('接收者 ID', 'watch.notify_user', w.notify_user, { mono: true, maxLength: 200, help: '需使用已确认的绑定；不要填写密钥。' })),
      notice(isSetup ? '初始化不会启用 QQ 实时接入。请先完成设置，再绑定接收者并显式启用渠道。QQ 官方适配器暂不支持主动通知，通知仅留存网页。' : '渠道试运行时仅记录通知预览，不真实外发。QQ 官方适配器暂不支持主动通知，会留存网页；需要主动 QQ 通知请配置 OneBot。Windows 上不可用的 Linux 指标不会伪装成正常值。'));
  }

  function environmentBlock(environment) {
    const e = object(environment); const wrap = node('div', 'environment-grid');
    const entries = [['主机名', e.hostname], ['操作系统 / 架构', [e.os, e.arch].filter(Boolean).join(' / ')], ['Go 运行时', e.go_version], ['Linux 系统工具', e.linux_tools === true ? '可用于 Linux 实机检查' : e.linux_tools === false ? '当前平台不支持 Linux 工具' : '未返回支持信息'], ['数据目录', e.data_dir], ['默认工作目录', e.work_dir]];
    entries.forEach(([label, value]) => { const item = node('div', 'environment-item'); item.append(node('span', 'environment-label', label), node('div', 'environment-value', value || '未提供')); wrap.append(item); });
    return wrap;
  }
  function wizardHeading(index, title, subtitle) { const wrap = node('div', 'wizard-heading'); wrap.append(node('span', 'section-index', `INITIAL SETUP / 0${index + 1}`), node('h1', '', title), node('p', '', subtitle)); return wrap; }
  function captureSetup(validate = true) {
    const form = $('#setup-form'); if (validate && !form.reportValidity()) return false;
    readFields(form, state.setup.config); Object.assign(state.setup.secrets, readSecrets(form));
    for (const key of ['official', 'onebot']) { state.setup.config.channels[key].enabled = false; state.setup.config.channels[key].dry_run = true; }
    state.setup.config.watch.enabled = false;
    return true;
  }
  function renderSetup() {
    if (!state.setup) return; const { step, config } = state.setup; const fields = $('#setup-fields'); clearSecrets(fields); fields.replaceChildren(); inlineError($('#setup-error'), null);
    $('#setup-progress').textContent = `0${step + 1} / 07`;
    const steps = STEPS.map((label, index) => { const n = node('div', `step-item${index === step ? ' active' : index < step ? ' done' : ''}`); if (index === step) n.setAttribute('aria-current', 'step'); const count = node('span', 'step-number'); count.append(index < step ? icon('check') : document.createTextNode(String(index + 1).padStart(2, '0'))); n.append(count, node('span', '', label)); return n; });
    $('#setup-steps').replaceChildren(...steps); $('#setup-back').disabled = step === 0; $('#setup-skip').hidden = step !== 1;
    $('#setup-next').replaceChildren(node('span', '', step === 6 ? '完成初始化' : '保存并继续'), icon(step === 6 ? 'check' : 'arrow'));
    if (step === 0) fields.append(wizardHeading(step, '先认识这台主机', '下面是服务端报告的真实环境。在身份验证前，这些信息不会公开。'), environmentBlock(state.environment), notice(state.environment.note || '确认主机与工作目录无误。系统命令的最终支持范围以服务器实际平台为准。', state.environment.os !== 'linux'));
    if (step === 1) fields.append(wizardHeading(step, '接上模型，不急着执行', '可以现在配置，也可以跳过。跳过后仍能查看主机指标与管理工作台，模型任务暂不可用。'), modelFields(config, true));
    if (step === 2) fields.append(wizardHeading(step, '定义一个可靠的搭档', '选一种适合你的表达方式，再决定它能记住什么。'), ...personaFields(config, true));
    if (step === 3) fields.append(wizardHeading(step, '把权限边界说清楚', '只授权实际需要的目录。目录外的访问会被后端拒绝。'), permissionFields(config));
    if (step === 4) fields.append(wizardHeading(step, '连接 QQ，先从试运行开始', '这一步只保存接入配置，不启用连接。也可以全部留空，稍后再配置。'), channelFields(config, true));
    if (step === 5) fields.append(wizardHeading(step, '需要值守时，再打开它', '初始化只保存巡检策略，开关保持关闭。完成设置后，在“自动值守”页确认并启用。'), watchFields(config, true));
    if (step === 6) {
      fields.append(wizardHeading(step, '准备就绪，最后核对一次', '完成后会生成访问令牌，只显示一次。请准备好密码管理器。'));
      const list = node('div', 'summary-list');
      const values = [['主机', state.environment.hostname || '未知'], ['工作目录', config.work_dir], ['模型', config.model.base_url && config.model.name && (state.setup.secrets.model_key || state.secretsConfigured.model_key) ? `${config.model.name} · 已填写密钥（未经测试不代表可用）` : '未完整配置 · 模型任务暂不可用'], ['助手', `${config.persona.name} / ${config.persona.tone}`], ['长期记忆', config.memory.enabled ? `启用 · 最多 ${config.memory.max_results} 条 / ${config.memory.max_chars} 字符` : '关闭'], ['目录授权', `读取 ${array(config.read_roots, 16).length} 个 · 写入 ${array(config.write_roots, 16).length} 个`], ['QQ 接入', '均不启用 · 保留试运行'], ['自动值守', config.watch.enabled ? `开启 · ${config.watch.interval_seconds} 秒采样` : '关闭']];
      values.forEach(([label, value]) => { const row = node('div', 'summary-row'); row.append(node('span', '', label), node('span', '', value)); list.append(row); }); fields.append(list);
      fields.append(notice('不会自动发出模型测试消息，不会在初始化阶段连接 QQ。API Key 和渠道凭据不会出现在摘要中。'));
      const ack = node('label', 'check-row'); const input = node('input'); input.type = 'checkbox'; input.id = 'setup-ack'; input.required = true;
      ack.append(input, node('span', '', '我已核对目录与权限，并会安全保存生成的访问令牌。')); fields.append(ack);
    }
    $('#setup-main-heading')?.remove(); window.scrollTo({ top: 0, behavior: 'instant' });
  }
  async function nextSetup() {
    if (!state.setup || !captureSetup()) return;
    if (state.setup.step !== 6) { state.setup.step++; renderSetup(); return; }
    const b = $('#setup-next'); b.disabled = true; $('#setup-back').disabled = true; inlineError($('#setup-error'), null);
    try { const result = await api('/api/setup/complete', { method: 'POST', body: { config: state.setup.config, secrets: state.setup.secrets } }); state.setup.secrets = {}; showToken(result.token); }
    catch (error) { if (!error.authHandled) inlineError($('#setup-error'), error); }
    finally { b.disabled = false; $('#setup-back').disabled = false; }
  }

  function applyConfig(payload, redraw = true) {
    if (!payload || !payload.config) throw new Error('配置响应缺少 config 字段。');
    state.config = payload.config; state.environment = object(payload.environment); state.presets = array(payload.presets, 20); state.secretsConfigured = object(payload.secrets_configured);
    $('#header-host').textContent = text(state.environment.hostname || '本地主机', 100);
    $('#settings-environment').replaceChildren(environmentBlock(state.environment));
    if (redraw) {
      renderPolicy(); renderModelNotice();
      ['settings', 'watch', 'channels'].forEach(key => { const form = $(`#${key}-config-form`); if (form.dataset.loaded && form.dataset.dirty !== '1') renderConfigForm(key); });
    }
  }
  async function enterApp(payload) {
    if (!payload) payload = await api('/api/config');
    state.phase = 'app'; state.authenticated = true; state.initialized = true; state.generation++; clearSecrets();
    applyConfig(payload, false); showScreen('app'); $('#app-version').textContent = `v${state.version || '—'} / LOCAL`;
    $('#overview-date').textContent = new Date().toLocaleDateString('zh-CN', { month: 'long', day: 'numeric', weekday: 'long' });
    renderMetrics(); renderPolicy(); renderModelNotice(); renderRail(); renderMessages();
    const hash = location.hash.slice(1); navigate(VIEWS[hash] ? hash : 'overview', false);
    await refreshAll(); if (!state.authenticated) return; connectEvents();
    state.metricTimer = window.setInterval(() => { if (!document.hidden && state.authenticated) { loadData('metrics'); loadData('brief'); if (!state.stream || state.stream.readyState !== EventSource.OPEN) queueRefresh('sessions', 'tasks', 'approvals', 'timeline', 'conversation'); } }, 30000);
  }
  function navigate(view, changeHash = true) {
    if (!VIEWS[view] || !state.authenticated) return;
    if (state.view !== view) state.contextOpen = false;
    state.view = view; $$('.view').forEach(v => { v.hidden = v.id !== `view-${view}`; });
    renderContextLayout();
    $$('.nav-item').forEach(b => { const active = b.dataset.view === view; b.classList.toggle('active', active); if (active) b.setAttribute('aria-current', 'page'); else b.removeAttribute('aria-current'); });
    $('#view-breadcrumb').textContent = VIEWS[view]; document.title = `${VIEWS[view]} · NEXUS / OPS`;
    if (changeHash && location.hash !== `#${view}`) history.replaceState(null, '', `#${view}`);
    closeNav();
    if (['settings', 'watch', 'channels'].includes(view)) { const form = $(`#${view}-config-form`); if (!form.dataset.loaded) renderConfigForm(view); }
    if (view === 'chat') { renderSessions(); renderMessages(); renderModelNotice(); if (state.selectedSession) loadConversation(); }
    if (view === 'approvals') loadData('approvals');
    if (view === 'memories') { renderMemories(); loadData('memories'); }
    if (view === 'timeline') { renderTimeline(); loadData('timeline'); }
    if (view === 'watch') { renderNotifications(); loadData('notifications'); }
    if (view === 'channels') { renderBindings(); loadData('channels'); loadData('bindings'); }
    renderRail();
  }
  function renderContextLayout() {
    const eligible = state.authenticated && ['chat', 'approvals'].includes(state.view);
    const inline = eligible && wideContext.matches;
    const open = eligible && !inline && state.contextOpen;
    $('.workspace-body').classList.toggle('with-context', inline);
    $('#app-screen').classList.toggle('context-open', open);
    $('#context-rail').hidden = !(inline || open);
    $('#context-toggle').hidden = !eligible || inline;
    $('#context-toggle').setAttribute('aria-expanded', String(inline || open));
    $('#context-backdrop').hidden = !open;
    $('#context-rail').setAttribute('aria-label', open ? '任务详情面板' : '任务上下文');
  }
  function closeContext(returnFocus = false) {
    state.contextOpen = false; renderContextLayout();
    if (returnFocus && !$('#context-toggle').hidden) $('#context-toggle').focus();
  }
  function closeNav() { $('#sidebar').classList.remove('open'); $('#nav-backdrop').hidden = true; $('#nav-toggle').setAttribute('aria-expanded', 'false'); }
  function connection(mode) {
    const el = $('#connection-status'); el.className = `connection-badge ${mode}`;
    $('span:last-child', el).textContent = mode === 'connected' ? '服务已连接' : mode === 'reconnecting' ? '事件流重连中' : '服务连接中断';
    $('#stream-indicator').classList.toggle('connected', mode === 'connected'); $('#stream-indicator').title = mode === 'connected' ? '事件流已连接' : '事件流暂未连接，数据可能滞后';
  }
  function connectEvents() {
    if (!state.authenticated) return; if (state.stream) state.stream.close();
    const stream = new EventSource(`/api/events?after=${state.eventID}`, { withCredentials: true }); state.stream = stream; connection('reconnecting');
    stream.addEventListener('open', () => { if (state.stream !== stream) return; connection('connected'); queueRefresh('sessions', 'tasks', 'approvals', 'timeline', 'conversation'); });
    stream.addEventListener('update', event => {
      if (state.stream !== stream || !state.authenticated || event.data.length > 1000000) return;
      let value; try { value = JSON.parse(event.data); } catch { return; }
      if (!value || typeof value !== 'object') return; const id = number(value.id || event.lastEventId); if (id !== null) state.eventID = Math.max(state.eventID, id);
      const kind = String(value.kind || '');
      if (kind === 'text' && value.session_id === state.selectedSession) {
        const data = eventData(value);
        if (typeof data.text === 'string' && value.task_id) { state.snapshots.set(value.task_id, { text: text(data.text, 65536), created_at: value.created_at, session_id: value.session_id, round: data.round }); if (state.snapshots.size > 20) state.snapshots.delete(state.snapshots.keys().next().value); }
      }
      if (kind === 'task' && !ACTIVE.has(eventData(value).status)) state.snapshots.delete(value.task_id);
      queueRefresh('tasks', 'approvals', 'timeline', 'conversation');
      if (['task', 'text'].includes(kind)) queueRefresh('sessions');
      if (kind === 'memory') queueRefresh('memories');
      if (kind === 'channel') queueRefresh('channels', 'bindings');
      if (kind === 'watch') queueRefresh('notifications', 'brief');
      if (kind === 'config') queueRefresh('config', 'channels', 'brief');
    });
    stream.addEventListener('error', () => {
      if (state.stream !== stream || !state.authenticated) return; connection('reconnecting');
      // EventSource cannot expose HTTP status. Probe a protected endpoint to detect 401.
      if (Date.now() - state.authProbe > 5000) { state.authProbe = Date.now(); loadData('config').then(ok => { if (!ok && state.authenticated) connection('offline'); }); }
    });
  }
  function queueRefresh(...keys) {
    if (!state.authenticated) return; keys.forEach(key => state.refreshSet.add(key));
    if (state.streamTimer) return;
    state.streamTimer = window.setTimeout(() => { const pending = [...state.refreshSet]; state.refreshSet.clear(); state.streamTimer = null; pending.forEach(key => key === 'conversation' ? loadConversation() : loadData(key)); }, 360);
  }
  const endpoints = { sessions: '/api/sessions', tasks: '/api/tasks', approvals: '/api/approvals', timeline: '/api/timeline', memories: '/api/memories', channels: '/api/channels', bindings: '/api/bindings', notifications: '/api/notifications', metrics: '/api/metrics', brief: '/api/brief', config: '/api/config' };
  async function loadData(key) {
    if (!state.authenticated || !endpoints[key]) return false;
    if (state.loading.has(key)) return state.loading.get(key);
    const generation = state.generation;
    const task = (async () => {
      try {
        const payload = await api(endpoints[key]); if (!state.authenticated || state.generation !== generation) return false;
        delete state.errors[key]; state.loaded[key] = true;
        if (key === 'config') applyConfig(payload);
        else if (key === 'metrics') { state.metrics = object(payload); state.metricsAt = new Date(); }
        else if (key === 'brief') state.brief = typeof payload.text === 'string' ? payload.text : '';
        else if (key === 'channels') state.channels = object(payload);
        else if (key === 'bindings') state.bindings = { bindings: array(payload.bindings), pending: array(payload.pending) };
        else state[key] = array(payload, key === 'memories' ? 200 : 100);
        if (key === 'timeline') for (const event of state.timeline) { const id = number(event.id); if (id !== null) state.eventID = Math.max(state.eventID, id); }
        renderData(key); return true;
      } catch (error) {
        if (error.authHandled || state.generation !== generation) return false;
        state.errors[key] = errorMessage(error); state.loaded[key] = true; renderData(key); return false;
      } finally { if (state.generation === generation) state.loading.delete(key); }
    })(); state.loading.set(key, task); return task;
  }
  function renderData(key) {
    if (!state.authenticated) return;
    if (key === 'metrics') renderMetrics();
    if (key === 'brief') $('#brief-text').textContent = text(state.errors.brief || state.brief || '服务尚未生成主机摘要。你可以先查看指标，或开始一次只读诊断。', 3000);
    if (key === 'sessions') renderSessions();
    if (key === 'tasks') { renderRecent(); renderActiveTasks(); }
    if (key === 'approvals') renderApprovals();
    if (key === 'memories') { if ($('#memory-search').value.trim()) searchMemories(); else renderMemories(); }
    if (key === 'timeline') { renderTimeline(); renderRail(); }
    if (key === 'channels') renderChannelStatuses();
    if (key === 'bindings') renderBindings();
    if (key === 'notifications') renderNotifications();
    renderPolicy();
  }
  async function refreshAll() {
    if (!state.authenticated) return;
    const b = $('#refresh-all'); b.disabled = true;
    try { await Promise.all(Object.keys(endpoints).filter(key => key !== 'config').map(loadData)); if (state.selectedSession) await loadConversation(); }
    finally { b.disabled = false; }
  }

  function renderMetrics() {
    const m = object(state.metrics); const cpu = object(m.cpu); const mem = object(m.memory); const disk = object(m.disk);
    const cpuPercent = firstNumber(m.cpu_percent, cpu.percent, cpu.usage_percent);
    const memoryPercent = firstNumber(m.memory_percent, mem.percent, mem.used_percent);
    const diskPercent = firstNumber(m.disk_percent, disk.percent, disk.used_percent);
    const unsupported = Array.isArray(m.unsupported) ? m.unsupported.join('、') : typeof m.unsupported === 'string' ? m.unsupported : '';
    const uptime = /uptime/i.test(unsupported) ? ['—', ''] : duration(firstNumber(m.uptime_seconds, m.uptime));
    const cpuOK = m.cpu_supported !== false && cpu.supported !== false && !/cpu/i.test(unsupported);
    const memOK = m.memory_supported !== false && mem.supported !== false && !/memory/i.test(unsupported);
    const diskOK = m.disk_supported !== false && disk.supported !== false && !/disk/i.test(unsupported);
    const memoryTotal = firstNumber(m.memory_total, m.memory_total_bytes, mem.total_bytes, mem.total);
    const diskTotal = firstNumber(m.disk_total, m.disk_total_bytes, disk.total_bytes, disk.total);
    const stats = [
      { label: 'CPU 使用率', icon: 'cpu', value: cpuOK ? cpuPercent : null, unit: '%', note: cpuPercent === null || !cpuOK ? '当前平台未提供有效采样' : `实时采样${firstNumber(m.cpu_cores, m.cpus, cpu.cores) !== null ? ` · ${firstNumber(m.cpu_cores, m.cpus, cpu.cores)} 核` : ''}`, tone: '' },
      { label: '内存使用率', icon: 'memory', value: memOK ? memoryPercent : null, unit: '%', note: memoryTotal !== null ? `总计 ${bytes(memoryTotal)}` : '等待平台内存指标', tone: 'blue' },
      { label: '磁盘使用率', icon: 'disk', value: diskOK ? diskPercent : null, unit: '%', note: diskTotal !== null ? `工作目录所在磁盘 · ${bytes(diskTotal)}` : '工作目录所在磁盘', tone: 'violet' },
      { label: '主机运行时间', icon: 'clock', value: uptime[0], unit: uptime[1], note: m.hostname || state.environment.hostname || '等待系统运行时间', duration: true, tone: '' }
    ];
    const cards = stats.map(item => {
      const card = node('div', 'metric-card'); const label = node('div', 'metric-label'); label.append(node('span', '', item.label), icon(item.icon));
      const valid = !state.errors.metrics && (item.duration ? item.value !== '—' : number(item.value) !== null && item.value >= 0 && item.value <= 100);
      const val = node('div', 'metric-value', valid ? item.duration ? item.value : Number(item.value).toFixed(1) : '—'); val.append(node('small', '', valid ? item.unit : ''));
      const meter = node('div', valid && !item.duration ? `metric-meter ${item.tone}` : 'metric-unavailable');
      if (valid && !item.duration) { const fill = node('span'); fill.style.width = `${Math.min(100, Math.max(0, item.value))}%`; meter.append(fill); }
      const note = node('div', 'metric-footnote', state.errors.metrics ? '采样失败 · 请刷新重试' : valid ? item.note : state.metrics ? (state.environment.os !== 'linux' ? '需 Linux 实机 · 当前不可用' : '暂无有效采样') : '等待首次采样');
      card.append(label, val, meter, note); return card;
    });
    $('#metric-cards').replaceChildren(...cards);
    const error = $('#metrics-error'); error.hidden = !state.errors.metrics;
    if (state.errors.metrics) error.replaceChildren(icon('alert'), node('p', '', state.errors.metrics), button('重试', 'ghost small', 'refresh', () => loadData('metrics')));
    $('#metrics-updated').textContent = state.errors.metrics ? '采样失败，未显示旧值' : state.metricsAt ? `读取于 ${state.metricsAt.toLocaleTimeString('zh-CN', { hour12: false })}` : '等待首次采样';
  }
  function renderRecent() {
    const root = $('#recent-tasks');
    if (state.errors.tasks) { root.replaceChildren(unavailable('tasks')); return; }
    if (!state.tasks.length) { root.replaceChildren(empty(state.loaded.tasks ? '还没有任务记录' : '正在读取任务', '从上方选一个检查目标，或进入任务对话描述问题。不会自动执行。', 'terminal', button('打开任务对话', 'secondary small', 'chat', () => navigate('chat')))); return; }
    root.replaceChildren(...state.tasks.slice(0, 5).map(task => {
      const row = node('div', 'task-row'); const mark = node('span', 'row-icon'); mark.append(icon('terminal'));
      const content = node('div', 'row-content'); const title = node('button', 'row-title', text(task.input || '未命名任务', 130)); title.type = 'button'; title.addEventListener('click', () => runAction(() => openTask(task)));
      content.append(title, node('small', '', `${shortID(task.id)}${task.dry_run ? ' · 试运行' : ''}`)); const time = node('time', '', date(task.created_at, true));
      row.append(mark, content, statusPill(task.status), time); return row;
    }));
  }
  async function openTask(task) { navigate('chat'); if (task.session_id) await selectSession(task.session_id); }
  function modelConfigured() { const m = object(state.config && state.config.model); return Boolean(m.base_url && m.name && state.secretsConfigured.model_key); }
  function readOnlySession() { return state.conversation && state.conversation.session && state.conversation.session.channel !== 'web'; }
  function renderModelNotice() {
    const configured = modelConfigured(); $('#model-notice').hidden = configured;
    const sessionLoading = Boolean(state.selectedSession && !state.conversation);
    $('#send-message').disabled = !configured || state.sending || sessionLoading || Boolean(readOnlySession());
    $('#message-input').disabled = sessionLoading || Boolean(readOnlySession());
    $('#message-input').placeholder = readOnlySession() ? 'QQ 会话仅供查看。请新建网页对话后发送任务。' : '描述你希望检查或完成的事，例如：为什么磁盘快满了？';
  }
  function renderSessions() {
    const root = $('#session-list'); $('#session-count').textContent = String(state.sessions.length);
    if (state.errors.sessions) { root.replaceChildren(unavailable('sessions')); return; }
    if (!state.sessions.length) { root.replaceChildren(empty(state.loaded.sessions ? '还没有对话' : '正在读取…', '点击右上角新建对话。', 'chat')); return; }
    root.replaceChildren(...state.sessions.slice(0, 100).map(session => {
      const b = node('button', `session-item${session.id === state.selectedSession ? ' active' : ''}`); b.type = 'button';
      if (session.id === state.selectedSession) b.setAttribute('aria-current', 'true');
      b.append(node('strong', '', session.title || '未命名对话'), node('small', '', `${session.channel === 'web' ? '网页' : channelName(session.channel)} · ${date(session.updated_at, true)}`));
      b.addEventListener('click', () => runAction(() => selectSession(session.id))); return b;
    }));
  }
  async function selectSession(id) {
    if (!id) return; state.drafts.set(state.selectedSession, $('#message-input').value);
    state.selectedSession = String(id); state.conversation = null; state.conversationEpoch++;
    $('#message-input').value = state.drafts.get(state.selectedSession) || ''; delete state.errors.conversation;
    renderSessions(); renderMessages(); renderRail(); await loadConversation();
  }
  async function loadConversation() {
    if (!state.authenticated || !state.selectedSession) return;
    const sessionID = state.selectedSession; const epoch = ++state.conversationEpoch; const generation = state.generation;
    try {
      const payload = await api(`/api/sessions/${encodeURIComponent(sessionID)}`);
      if (!state.authenticated || state.generation !== generation || state.selectedSession !== sessionID || state.conversationEpoch !== epoch) return;
      state.conversation = { session: object(payload.session), messages: array(payload.messages, 100), tasks: array(payload.tasks, 100) }; delete state.errors.conversation;
      for (const [taskID, snap] of state.snapshots) { if (snap.session_id === sessionID && state.conversation.messages.some(m => m.role === 'assistant' && text(m.content, 65536) === snap.text)) state.snapshots.delete(taskID); }
      renderMessages(); renderRail();
    } catch (error) { if (error.authHandled || state.generation !== generation || state.selectedSession !== sessionID || epoch !== state.conversationEpoch) return; state.errors.conversation = errorMessage(error); renderMessages(); }
  }
  function renderMessages() {
    const root = $('#messages'); const nearBottom = root.scrollHeight - root.scrollTop - root.clientHeight < 100; const priorScroll = root.scrollTop;
    const conversation = state.conversation; $('#conversation-title').textContent = text(conversation ? conversation.session.title || '未命名对话' : state.selectedSession ? '正在读取对话…' : '新的运维对话', 160);
    $('#conversation-subtitle').textContent = conversation ? `${conversation.session.channel === 'web' ? '网页任务' : `${channelName(conversation.session.channel)} · 仅供查看`} · ${text(conversation.session.scope || 'owner', 100)}` : '网页任务 · 操作由你确认';
    $('#delete-session').disabled = !state.selectedSession || Boolean(conversation && conversation.tasks.some(t => ACTIVE.has(t.status)));
    const error = $('#conversation-error'); error.hidden = !state.errors.conversation;
    if (state.errors.conversation) error.replaceChildren(icon('alert'), node('p', '', state.errors.conversation), button('重新读取', 'ghost small', 'refresh', loadConversation));
    const messages = conversation ? conversation.messages : [];
    if (!messages.length) {
      const placeholder = empty(state.selectedSession && !conversation ? '正在读取对话' : '从一个问题开始', state.selectedSession && !conversation ? '正在加载已保存的记录。' : '描述你想检查的事情。需要修改时，助手会先等你确认。', 'logo'); placeholder.classList.add('chat-empty');
      if (!state.selectedSession || conversation) { const suggestions = node('div', 'chat-suggestions'); [['主机健康检查', '请对当前主机做一次只读健康检查，列出真实数据支持的发现。'], ['排查磁盘占用', '请排查工作目录所在磁盘的空间占用，先分析，不要删除文件。']].forEach(([label, prompt]) => suggestions.append(button(label, 'ghost small', null, () => setPrompt(prompt)))); placeholder.append(suggestions); }
      root.replaceChildren(placeholder);
    } else {
      root.replaceChildren(...messages.filter(m => ['user', 'assistant', 'tool'].includes(m.role) && String(m.content || '').length).map(m => messageNode(m)));
    }
    if (conversation) for (const [taskID, snap] of state.snapshots) { if (snap.session_id === state.selectedSession && conversation.tasks.some(t => t.id === taskID && ACTIVE.has(t.status)) && !messages.some(m => m.role === 'assistant' && text(m.content, 65536) === snap.text)) root.append(messageNode({ role: 'assistant', content: snap.text, created_at: snap.created_at }, true)); }
    if (nearBottom || !messages.length) root.scrollTop = root.scrollHeight; else root.scrollTop = priorScroll;
    renderActiveTasks(); renderModelNotice();
  }
  function messageNode(message, streaming = false) {
    const wrap = node('article', `message ${message.role}${streaming ? ' streaming' : ''}`); const avatar = node('span', 'message-avatar');
    avatar.append(message.role === 'user' ? document.createTextNode('你') : icon(message.role === 'tool' ? 'terminal' : 'logo'));
    const body = node('div', 'message-body'); const meta = node('div', 'message-meta');
    meta.append(node('strong', '', message.role === 'user' ? '你' : message.role === 'tool' ? '工具输出' : state.config?.persona?.name || 'Ops 助手'), node('time', '', date(message.created_at, true)));
    const content = node(message.role === 'tool' ? 'pre' : 'div', message.role === 'tool' ? 'output-block' : 'message-text', text(message.content, message.role === 'tool' ? 24000 : 32000));
    body.append(meta, content); wrap.append(avatar, body); return wrap;
  }
  function renderActiveTasks() {
    const root = $('#active-tasks'); if (!state.selectedSession) { root.replaceChildren(); return; }
    const tasks = state.tasks.filter(t => t.session_id === state.selectedSession); const source = tasks.length ? tasks : array(state.conversation && state.conversation.tasks);
    const relevant = source.filter(t => ACTIVE.has(t.status)).slice(0, 8);
    root.replaceChildren(...relevant.map(task => {
      const row = node('div', 'active-task'); row.append(statusPill(task.status), node('span', '', text(task.input || `任务 ${task.id}`, 100)));
      row.append(iconButton('取消此任务', 'stop', () => cancelTask(task))); return row;
    }));
    if (relevant.length) $('#delete-session').disabled = true;
  }
  function setPrompt(prompt) {
    navigate('chat'); if (readOnlySession()) { state.selectedSession = ''; state.conversation = null; renderMessages(); renderSessions(); }
    $('#message-input').value = prompt; state.drafts.set(state.selectedSession, prompt); $('#message-input').focus();
  }
  function newSession() {
    const title = field('对话名称', null, '', { maxLength: 70, placeholder: '例如：站点运行排查（可留空）' });
    openDialog({ title: '新建运维对话', body: [node('p', '', '新建对话不会调用模型或执行工具。'), title], confirm: '创建对话', onSubmit: async () => {
      const result = await api('/api/sessions', { method: 'POST', body: { title: $('input', title).value.trim() } });
      if (!result.id) throw new Error('服务没有返回会话 ID。'); await loadData('sessions'); await selectSession(result.id); navigate('chat'); toast('对话已创建。');
    } });
  }
  async function sendTask(event) {
    event.preventDefault(); if (state.sending || !modelConfigured() || readOnlySession()) return;
    const input = $('#message-input'); const message = input.value.trim(); if (!message) { input.focus(); return; }
    if ([...message].length > 12000) { toast('任务描述最多 12,000 个字符，请缩短后发送。', true); return; }
    state.sending = true; renderModelNotice(); const sessionID = state.selectedSession;
    try {
      const task = await api('/api/tasks', { method: 'POST', body: { session_id: sessionID, message } });
      if (!task || !task.id || !task.session_id) throw new Error('服务未返回任务标识，请刷新后核实是否提交成功。');
      if (input.value.trim() === message && state.selectedSession === sessionID) input.value = '';
      state.drafts.delete(sessionID); state.tasks = [task, ...state.tasks.filter(t => t.id !== task.id)].slice(0, 100);
      if (!sessionID) { state.selectedSession = task.session_id; state.conversation = null; }
      renderActiveTasks(); await Promise.all([loadData('sessions'), loadData('tasks'), loadConversation()]);
    } catch (error) { if (!error.authHandled) toast(errorMessage(error), true); }
    finally { state.sending = false; if (state.authenticated) { renderModelNotice(); if (!input.disabled) input.focus(); } }
  }
  function cancelTask(task) {
    confirmAction('停止这个任务？', '将请求后端取消后续执行。已经完成的操作不会自动撤销，正在执行的命令以实际停止结果为准。', async () => {
      await api(`/api/tasks/${encodeURIComponent(task.id)}/cancel`, { method: 'POST', body: {} }); toast('已提交取消请求，请以任务最终状态为准。'); queueRefresh('tasks', 'approvals', 'conversation', 'timeline');
    }, { confirm: '请求停止', danger: true, preview: task.input });
  }
  function deleteSession() {
    const id = state.selectedSession; if (!id) return;
    confirmAction('清除此对话历史？', '会永久删除这个会话的消息、任务与关联时间线，不会删除长期记忆。活动任务必须先停止；此操作不可撤销。', async () => {
      await api(`/api/sessions/${encodeURIComponent(id)}`, { method: 'DELETE', body: {} }); state.drafts.delete(id);
      if (state.selectedSession === id) { state.selectedSession = ''; state.conversation = null; $('#message-input').value = ''; }
      state.snapshots.clear(); renderMessages(); await Promise.all(['sessions', 'tasks', 'timeline'].map(loadData)); toast('对话历史已清除。');
    }, { confirm: '永久清除历史', danger: true });
  }

  function renderApprovals() {
    $('#nav-approval-count').hidden = !state.approvals.length; $('#nav-approval-count').textContent = String(state.approvals.length);
    $('#approvals-total').textContent = state.errors.approvals ? '读取失败' : `${state.approvals.length} 项待确认`; const root = $('#approval-list');
    if (state.errors.approvals) { root.replaceChildren(unavailable('approvals')); renderRail(); return; }
    if (!state.approvals.length) { root.replaceChildren(empty(state.loaded.approvals ? '目前没有待审批操作' : '正在读取待审批操作', '需要授权时，任务会停在这里。只有你核对并批准后，后端才会继续。', 'shield')); renderRail(); return; }
    root.replaceChildren(...state.approvals.slice(0, 100).map(item => {
      const card = node('article', 'panel approval-card'); const header = node('div', 'approval-header'); const title = node('h3'); title.append(icon(item.kind === 'memory' ? 'memory' : 'terminal'), node('span', '', item.name || '待确认操作'));
      header.append(title, node('time', '', date(item.created_at, true))); const meta = node('div', 'approval-meta'); meta.append(pill(item.kind === 'memory' ? '长期记忆写入' : '具体工具操作', 'amber'), node('span', 'mono', `任务 ${shortID(item.task_id, 12)}`));
      const preview = typeof item.preview === 'string' ? item.preview : stringify(item.preview, 65536);
      const actions = node('div', 'approval-actions'); actions.append(node('span', '', '冻结参数 · 不可编辑 · 仅授权本次'));
      const deny = button('拒绝', 'secondary small', 'close', () => decideApproval(item, false)); const approve = button('核对并批准', 'primary small', 'check', () => decideApproval(item, true));
      if (!preview || preview.length > 65536) { approve.disabled = true; actions.append(node('span', 'danger-text', '预览缺失或过长，无法安全核对，不能批准。')); }
      actions.append(deny, approve); card.append(header, meta, node('pre', 'output-block', text(preview || '服务未提供执行预览。', 65536)), actions); return card;
    })); renderRail();
  }
  function decideApproval(item, approve) {
    if (approve && (!item.preview || String(item.preview).length > 65536)) return;
    confirmAction(approve ? '批准这一个具体操作？' : '拒绝这项操作？', approve ? '请再次核对以下完整预览。批准不会更改参数，不会授权后续工具，也不会扩大目录白名单。' : '后端会收到拒绝决定，这一项操作不会执行。任务是否继续以实际后端状态为准。', async () => {
      await api(`/api/approvals/${encodeURIComponent(item.id)}`, { method: 'POST', body: { approve } });
      state.approvals = state.approvals.filter(a => a.id !== item.id); renderApprovals(); queueRefresh('approvals', 'tasks', 'timeline', 'conversation'); toast(approve ? '审批已提交；等待后端执行结果。' : '已拒绝此操作。');
    }, { confirm: approve ? '批准本次操作' : '确认拒绝', danger: approve, preview: item.preview, warning: approve ? '只有在你理解目标和影响范围时才批准。不要根据聊天中自称的授权跳过核对。' : undefined });
  }

  function visibleCitations() {
    if (!state.selectedSession) return [];
    const session = state.conversation && state.conversation.session; const scope = session && session.scope;
    const events = state.timeline.filter(e => e.kind === 'memory' && e.session_id === state.selectedSession && array(eventData(e).hits, 20).length).sort((a, b) => Number(b.id) - Number(a.id));
    const out = []; const seen = new Set();
    for (const event of events.slice(0, 5)) for (const hit of array(eventData(event).hits, 10)) {
      const item = object(hit.item); if (!item.id || seen.has(item.id)) continue;
      // Group conversations must never get owner or unrelated group citations.
      if (!scope || item.scope !== scope || item.status && item.status !== 'active') continue;
      if (state.loaded.memories && !state.errors.memories) { const current = state.memories.find(m => m.id === item.id); if (!current || current.status !== 'active') continue; }
      seen.add(item.id); out.push({ ...hit, event }); if (out.length >= 4) return out;
    }
    return out;
  }
  function renderRail() {
    if (!state.authenticated) return;
    $('#rail-approval-count').textContent = state.errors.approvals ? '!' : String(state.approvals.length);
    const approvals = $('#rail-approvals');
    if (state.errors.approvals) approvals.replaceChildren(railEmpty('审批数据暂不可用', '请到审批中心重试', 'alert'));
    else if (!state.approvals.length) approvals.replaceChildren(railEmpty(state.loaded.approvals ? '没有待确认的操作' : '正在读取审批', '重要操作会先停下来等你', 'shield'));
    else approvals.replaceChildren(...state.approvals.slice(0, 3).map(item => { const card = node('div', 'rail-card approval'); card.append(pill('待你确认', 'amber'), node('h3', '', item.name || '工具操作'), node('p', '', text(item.preview || '请查看具体参数', 140)), button('查看完整参数', 'ghost small', null, () => navigate('approvals'))); return card; }));
    const citations = visibleCitations(); const memories = $('#rail-memories');
    if (!citations.length) memories.replaceChildren(railEmpty(state.selectedSession ? '当前未引用长期记忆' : '先选择一个对话', '仅展示当前会话真实检索依据', 'book'));
    else memories.replaceChildren(...citations.map(hit => { const item = hit.item; const card = node('div', 'rail-card'); card.append(node('h3', '', item.subject || categoryName(item.category)), node('p', '', text(item.text, 220)), node('div', 'tiny', `来源 ${text(item.source || '未提供', 50)} · ${date(item.updated_at, true)}`), button('查看关联引用', 'ghost small', null, () => showMemoryReferences(item.id))); return card; }));
  }
  function railEmpty(title, detail, symbol) { const wrap = node('div', 'rail-empty'); wrap.append(icon(symbol), node('span', '', title), node('small', '', detail)); return wrap; }
  function renderPolicy() {
    if (!state.config) return; const c = state.config;
    const items = [['模型', modelConfigured() ? `${c.model.name} · 已配置` : '未配置'], ['长期记忆', c.memory.enabled ? `最多 ${c.memory.max_results} 条引用` : '已关闭'], ['执行审批', '网页逐项确认'], ['自动值守', c.watch.enabled ? `${c.watch.interval_seconds}s / 次` : '已关闭'], ['目录边界', `${array(c.read_roots, 16).length} 读 / ${array(c.write_roots, 16).length} 写`]];
    $('#rail-policy').replaceChildren(...items.map(([label, value]) => { const row = node('div', 'policy-row'); row.append(node('span', '', label), node('span', '', value)); return row; }));
    $('#watch-state').textContent = c.watch.enabled ? '已启用' : '已关闭'; $('#watch-state').className = `pill ${c.watch.enabled ? 'green' : ''}`;
  }

  function categoryName(value) { return ({ fact: '事实资料', preference: '个人偏好', procedure: '处理经验', incident: '故障记录', note: '备忘' })[value] || value || '未分类'; }
  function renderMemories() {
    const root = $('#memory-list'); const q = $('#memory-search').value.trim(); const filter = $('#memory-filter').value;
    if (state.errors.memories && !q) { root.replaceChildren(unavailable('memories')); $('#memory-count').textContent = '记忆读取失败'; return; }
    const hits = q ? state.memoryHits : state.memories.map(item => ({ item }));
    if (q && hits === null) { root.replaceChildren(empty('正在检索', '检索只在已授权范围内进行，相关度不代表内容一定正确。', 'search')); return; }
    const shown = array(hits, 200).filter(hit => hit.item && (filter === 'all' || hit.item.status === filter));
    $('#memory-count').textContent = `${q ? '找到' : '全部记忆'} ${shown.length} 条${q ? ' · 按相关度排序' : ''}`;
    if (!shown.length) { root.replaceChildren(empty(q ? '没有匹配的记忆' : '这里还没有可显示的记忆', q ? '尝试更具体的主机名、主题或关键词；检索只返回使用中的可访问资料。' : '用“新增记忆”保存一条事实或偏好。不会自动从聊天中学习。', 'memory', q ? undefined : button('新增一条记忆', 'secondary small', 'plus', () => editMemory()))); return; }
    root.replaceChildren(...shown.map(hit => {
      const item = object(hit.item); const card = node('article', 'memory-card');
      const mark = node('span', 'memory-mark'); mark.append(icon(item.category === 'preference' ? 'chat' : item.category === 'incident' ? 'alert' : 'book'));
      const main = node('div', 'memory-main'); const header = node('div', 'memory-card-header');
      header.append(node('h3', 'memory-category', item.subject || categoryName(item.category)));
      if (item.subject) header.append(node('span', 'memory-kind', categoryName(item.category)));
      const sources = { user: '手动添加', 'user-confirmed': '确认保存', import: '导入资料' };
      const metadata = node('div', 'memory-metadata');
      const scope = node('span', '', item.scope === 'owner' ? '仅自己' : String(item.scope || '').startsWith('group:') ? '指定群聊' : '范围未知');
      scope.prepend(icon('lock')); scope.title = text(item.scope || '未知', 300);
      metadata.append(scope, node('span', '', sources[item.source] || '资料记录'), node('time', '', `更新于 ${date(item.updated_at, true)}`));
      if (item.expires_at) metadata.append(node('span', '', `到期 ${date(item.expires_at, true)}`));
      if (item.superseded_by) { const replacement = node('span', 'mono', `替代项 ${shortID(item.superseded_by)}`); replacement.title = text(item.superseded_by, 128); metadata.append(replacement); }
      main.append(header, node('p', 'memory-text', text(item.text, 6000)), metadata);
      if (hit.why) main.append(node('div', 'why', `匹配：${Array.isArray(hit.why) ? hit.why.join('、') : hit.why}${number(hit.score) !== null ? ` · 相关度 ${Number(hit.score).toFixed(2)}` : ''}`));
      const actions = node('div', 'memory-card-actions');
      actions.append(button('编辑', 'ghost small', 'edit', () => editMemory(item)), button('状态', 'ghost small', null, () => changeMemoryState(item)), iconButton('查看引用', 'link', () => showMemoryReferences(item.id)), iconButton('删除这条记忆', 'trash', () => deleteMemory(item), 'danger-text'));
      const side = node('div', 'memory-card-side'); side.append(statusPill(item.status), actions);
      card.append(mark, main, side); return card;
    }));
  }
  async function searchMemories() {
    const q = $('#memory-search').value.trim(); const epoch = ++state.memorySearchEpoch;
    if (!q) { state.memoryHits = null; renderMemories(); return; }
    state.memoryHits = null; renderMemories();
    try { const hits = await api(`/api/memories/search?q=${encodeURIComponent(q)}`); if (epoch !== state.memorySearchEpoch || !state.authenticated) return; state.memoryHits = array(hits, 50); renderMemories(); }
    catch (error) { if (epoch !== state.memorySearchEpoch || error.authHandled) return; $('#memory-count').textContent = '检索失败'; $('#memory-list').replaceChildren(empty('检索暂未完成', errorMessage(error), 'alert', button('重新检索', 'secondary small', 'refresh', searchMemories))); }
  }
  function editMemory(item = {}) {
    const wrap = node('div'); const form = grid(
      field('主题', 'subject', item.subject || '', { maxLength: 160 }),
      field('分类', 'category', item.category || 'fact', { options: [...new Set(['fact', 'preference', 'procedure', 'incident', 'note', item.category].filter(Boolean))].map(key => [key, categoryName(key)]) }),
      field('记忆内容', 'text', item.text || '', { multiline: true, rows: 5, maxLength: 1000, required: true, wide: true, help: '最多 1000 字符。记录事实或偏好，不要填写密码、API Key、令牌或私钥。' }),
      field('使用范围', 'scope', item.scope || 'owner', { required: true, maxLength: 256, help: 'owner 为所有者私有；群资料格式为 group:<channel>:<bot>:<group>，不能跨群引用。' }),
      field('重要度 / 0–1', 'importance', item.importance ?? .6, { type: 'number', min: 0, max: 1, step: .05, required: true }),
      field('状态', 'status', item.status || 'active', { options: [['active', '使用中'], ['archived', '已归档'], ['superseded', '已替代']] }),
      field('替代记忆 ID（可选）', 'superseded_by', item.superseded_by || '', { maxLength: 64, mono: true, help: '已替代状态可关联同一使用范围内的新记忆 ID。' }),
      field('到期时间（可选）', 'expires_at', item.expires_at || '', { maxLength: 80, wide: true, placeholder: '2027-01-01T00:00:00Z', help: 'ISO 8601 / RFC3339 格式，留空表示无到期时间。' }));
    wrap.append(form); const ack = node('label', 'check-row'); const input = node('input'); input.type = 'checkbox'; input.required = true;
    ack.append(input, node('span', '', '我确认保存这条资料，内容不包含凭据，且范围配置正确。')); wrap.append(ack);
    openDialog({ title: item.id ? '编辑长期记忆' : '新增长期记忆', eyebrow: 'EXPLICIT MEMORY', body: wrap, confirm: '确认保存记忆', wide: true, onSubmit: async () => {
      const payload = readFields(form, {}); payload.source = 'user';
      if (item.id) { payload.id = item.id; if (item.updated_at) payload.updated_at = item.updated_at; }
      if (payload.status !== 'superseded') payload.superseded_by = '';
      if (payload.expires_at && Number.isNaN(new Date(payload.expires_at).getTime())) throw new Error('到期时间不是有效的 ISO 8601 时间。');
      payload.expires_at = payload.expires_at ? new Date(payload.expires_at).toISOString() : null;
      await api('/api/memories', { method: 'POST', body: payload }); await loadData('memories'); queueRefresh('timeline'); toast('记忆已由你确认保存。');
    } });
  }
  function changeMemoryState(item) {
    const wrap = node('div'); const form = grid(field('新的状态', 'status', item.status, { options: [['active', '使用中'], ['archived', '已归档'], ['superseded', '已替代']] }), field('替代项 ID（可选）', 'superseded_by', item.superseded_by || '', { maxLength: 64, mono: true, help: '只允许关联同一使用范围内的其他记忆。' }));
    wrap.append(node('p', 'section-description', '归档或替代后不再参与新的检索。重新启用应确认内容仍然有效。'), form, node('pre', 'output-block', text(item.text, 2500)));
    openDialog({ title: '修改记忆状态', body: wrap, confirm: '确认修改', onSubmit: async () => {
      const payload = readFields(form, {}); if (payload.status !== 'superseded') payload.superseded_by = '';
      await api(`/api/memories/${encodeURIComponent(item.id)}/state`, { method: 'POST', body: payload }); await loadData('memories'); renderRail(); toast('记忆状态已更新。');
    } });
  }
  function deleteMemory(item) {
    confirmAction('永久删除这条记忆？', '内容将从工作台记忆中删除，不能从网页撤销。相关引用和模型历史由后端清理；这不代表磁盘或外部备份已安全擦除，正在使用记忆的任务可能被取消。', async () => {
      await api(`/api/memories/${encodeURIComponent(item.id)}`, { method: 'DELETE', body: {} }); await Promise.all(['memories', 'timeline', 'tasks'].map(loadData)); renderRail(); toast('记忆已删除。');
    }, { confirm: '永久删除', danger: true, preview: text(item.text, 3000) });
  }
  function showMemoryReferences(id) {
    const found = state.timeline.filter(event => event.kind === 'memory' && array(eventData(event).hits, 20).some(hit => hit.item && hit.item.id === id));
    const wrap = node('div');
    if (!found.length) wrap.append(empty('最近事件中没有找到引用', '这里只检查服务端返回的最近时间线，不代表这条记忆从未被使用。', 'book'));
    else found.slice(0, 15).forEach(event => {
      const item = node('div', 'memory-citation'); const hit = array(eventData(event).hits, 20).find(h => h.item && h.item.id === id);
      item.append(node('p', '', `任务 ${event.task_id || '未关联任务'}`), node('small', '', `来源 ${hit.item.source || '未知'} · 资料更新 ${date(hit.item.updated_at, true)}`), node('small', '', `引用于 ${date(event.created_at)} · 范围 ${hit.item.scope || '未知'}`));
      if (event.session_id) item.append(button('打开关联对话', 'ghost small', 'arrow', async () => { closeDialog(true); navigate('chat'); await selectSession(event.session_id); })); wrap.append(item);
    });
    openDialog({ title: '记忆关联引用', body: wrap, confirm: '已查看', onSubmit: async () => {} });
  }

  function kindName(kind) { return ({ task: '任务状态', text: '助手输出', tool: '工具调用', approval: '操作审批', memory: '记忆依据', channel: '接入状态', watch: '自动值守', config: '配置更新' })[kind] || kind || '系统事件'; }
  function renderTimeline() {
    const root = $('#timeline-list'); const filter = $('#timeline-filter').value;
    if (state.errors.timeline) { root.replaceChildren(unavailable('timeline')); return; }
    const events = state.timeline.filter(e => filter === 'all' || e.kind === filter).slice(0, 100);
    if (!events.length) { root.replaceChildren(empty(state.loaded.timeline ? '这里还没有相关事件' : '正在读取时间线', '执行任务、完成审批或修改设置后，真实事件会出现在这里。', 'clock')); return; }
    root.replaceChildren(...events.map(event => {
      const data = eventData(event); const entry = node('article', 'timeline-event'); const header = node('div', 'timeline-event-header');
      header.append(pill(kindName(event.kind), event.kind === 'approval' ? 'amber' : event.kind === 'memory' ? 'violet' : ''), node('strong', '', data.name || data.action || ''), node('time', '', date(event.created_at, true)));
      if (data.status) header.append(statusPill(data.status)); entry.append(header);
      let summary = data.message || data.error || data.text || data.input || data.preview || '';
      if (event.kind === 'approval' && typeof data.approved === 'boolean') summary = data.approved ? '用户已批准本次操作。批准不等于已执行成功。' : '用户已拒绝本次操作。';
      if (event.kind === 'memory' && Array.isArray(data.hits)) summary = `检索返回 ${data.hits.length} 条资料。相关度仅表示文本匹配，记忆不授予操作权限。`;
      if (summary) entry.append(node('p', '', text(typeof summary === 'string' ? summary : stringify(summary), 700)));
      const details = node('details'); details.append(node('summary', '', '查看可见事件数据'), node('pre', 'output-block', stringify(data, 18000))); entry.append(details);
      if (event.session_id) entry.append(button('关联对话', 'ghost small', 'arrow', async () => { navigate('chat'); await selectSession(event.session_id); })); return entry;
    }));
  }

  function savebar(label) { const wrap = node('div', 'form-savebar'); const save = button(label, 'primary', 'check'); save.type = 'submit'; wrap.append(node('p', '', '保存前需重新输入访问令牌。留空的凭据会保留原值。'), save); return wrap; }
  function renderConfigForm(kind) {
    if (!state.config) return; const form = $(`#${kind}-config-form`); clearSecrets(form); form.replaceChildren();
    if (kind === 'settings') form.append(modelFields(state.config), ...personaFields(state.config), permissionFields(state.config), limitFields(state.config), savebar('保存工作台设置'));
    if (kind === 'channels') form.append(channelFields(state.config), savebar('保存接入配置'));
    if (kind === 'watch') form.append(watchFields(state.config), savebar('保存值守策略'));
    form.dataset.loaded = '1'; delete form.dataset.dirty;
    if (!form.dataset.wired) { form.addEventListener('input', () => { form.dataset.dirty = '1'; }); form.addEventListener('change', () => { form.dataset.dirty = '1'; }); form.addEventListener('submit', event => { event.preventDefault(); runAction(() => saveConfig(kind)); }); form.dataset.wired = '1'; }
  }
  function validateConfigDraft(config) {
    if (array(config.read_roots, 100).length < 1 || array(config.read_roots, 100).length > 16 || array(config.write_roots, 100).length > 16) throw new Error('读取目录需要 1–16 个，写入目录最多 16 个。');
    for (const key of ['official', 'onebot']) if (array(config.channels[key].groups, 100).length > 32) throw new Error('单个渠道最多授权 32 个群。');
    if (config.channels.onebot.enabled && (!config.channels.onebot.bot_id || !config.channels.onebot.url)) throw new Error('启用 OneBot 前请填写预期机器人 ID 与 WebSocket 地址。');
    if (config.channels.official.enabled && !config.channels.official.app_id) throw new Error('启用 QQ 官方接入前请填写 AppID。');
    if (config.watch.notify_channel && config.watch.enabled && !config.watch.notify_user) throw new Error('启用外部值守通知时，请填写已绑定的接收者 ID。');
  }
  async function saveConfig(kind) {
    const form = $(`#${kind}-config-form`); if (!form.reportValidity()) return;
    // Rebase the edited section onto a fresh backend config, avoiding stale unrelated settings.
    const payload = await api('/api/config'); if (!payload.config) throw new Error('未能读取最新配置，未保存。');
    const config = readFields(form, clone(payload.config)); validateConfigDraft(config);
    const body = [node('p', '', `即将更新${VIEWS[kind]}。这会修改服务端配置；请使用访问令牌确认，不要通过聊天或 QQ 提交令牌。`)];
    const enabledChannels = ['official', 'onebot'].filter(key => config.channels[key].enabled);
    if (enabledChannels.length) body.push(notice(`本次保存包含已启用的 ${enabledChannels.map(channelName).join('、')}，后端可能立即建立或重建外部连接。${enabledChannels.some(key => !config.channels[key].dry_run) ? '其中有实时渠道：可能发送真实业务消息和通知。' : '当前均为试运行：业务消息不会外发，但仍会连接外部服务。'}`, true));
    if (config.watch.enabled) body.push(notice(`自动值守已开启，每 ${config.watch.interval_seconds} 秒采样，${config.watch.notify_channel ? '有外部通知目标，请核对绑定与试运行状态。' : '仅在工作台记录。'}`));
    const tokenField = field('再次输入访问令牌', null, '', { type: 'password', required: true, maxLength: 512, placeholder: '不是模型 API Key，也不是设置码' }); body.push(tokenField);
    openDialog({ title: '确认保存配置', eyebrow: 'OWNER CONFIRMATION', body, confirm: '确认并保存', onSubmit: async () => {
      let token = $('input', tokenField).value.trim(); $('input', tokenField).value = ''; let secrets = readSecrets(form);
      try {
        await api('/api/config', { method: 'PUT', body: { config, secrets, confirm_token: token } });
        token = ''; secrets = {}; clearSecrets(form); delete form.dataset.dirty;
        const latest = await api('/api/config'); applyConfig(latest); renderConfigForm(kind); queueRefresh('channels', 'timeline', 'notifications'); toast('配置已保存。连接是否可用请以实际状态或手动测试为准。');
      } finally { token = ''; secrets = {}; clearSecrets(form); }
    } });
  }
  function testModel(root, isSetup) {
    if (isSetup && !$('#setup-form').reportValidity()) return;
    const base = $('[data-path="model.base_url"]', root).value.trim(); const name = $('[data-path="model.name"]', root).value.trim();
    if (!base || !name) { toast('请先填写 API 地址和模型名称。', true); return; }
    const result = $('.test-result', root); const body = [node('p', '', '确认后，服务器会使用所填配置（空密钥时使用已保存的密钥）向这个模型服务发送一次固定测试请求，可能计入服务商用量。不会附带主机信息、历史对话或记忆。'), node('pre', 'output-block', `目标：${base}\n模型：${name}\n\n固定消息：Connection test. Reply OK only.`)];
    openDialog({ title: '允许向模型发送测试消息？', eyebrow: 'EXPLICIT OUTBOUND TEST', body, confirm: '发送一次测试', onSubmit: async () => {
      result.className = 'test-result muted'; result.textContent = '正在等待模型接口响应…';
      let key = $('[data-secret="model_key"]', root).value.trim() || (isSetup ? state.setup?.secrets.model_key || '' : '');
      try {
        const response = await api('/api/test/model', { method: 'POST', body: { model: { base_url: base, name }, api_key: key }, timeout: 40000 });
        if (response.ok !== true) throw new Error('服务没有返回成功标志，不能认定测试通过。');
        result.className = 'test-result form-success'; result.textContent = `服务端确认测试成功。模型返回：${text(response.reply || '（无可见文本）', 1200)}\n本次测试不会保存配置。`;
      } catch (error) { result.className = 'test-result form-error'; result.textContent = errorMessage(error); throw error; }
      finally { key = ''; }
    } });
  }
  function channelName(key) { return ({ official: 'QQ 官方', onebot: 'OneBot', web: '网页' })[key] || key || '未知渠道'; }
  function channelPill(key) {
    const c = object(state.channels[key]);
    if (state.errors.channels) return pill('状态读取失败', 'red');
    if (c.status) return statusPill(c.status);
    if (c.enabled === false) return pill('未启用');
    return pill(state.loaded.channels ? '未返回状态' : '读取中');
  }
  function renderChannelStatuses() { $$('[data-channel-status]').forEach(holder => holder.replaceChildren(channelPill(holder.dataset.channelStatus))); }
  function testChannel(key, card) {
    const form = $('#channels-config-form'); const result = $('.test-result', card);
    confirmAction(`测试 ${channelName(key)} 的已保存连接？`, `${form.dataset.dirty === '1' ? '页面存在尚未保存的更改，本次不会使用它们。\n\n' : ''}将使用服务端已保存的地址与凭据进行外部连通性检查，不发送业务消息。测试结果并不保证实际机器人权限、绑定或群授权均正确。`, async () => {
      result.className = 'test-result muted'; result.textContent = '正在等待服务端连接检查…';
      try { const response = await api(`/api/channels/${key}/test`, { method: 'POST', body: {}, timeout: 30000 }); if (response.ok !== true) throw new Error('服务未确认检查通过。'); result.className = 'test-result form-success'; result.textContent = '服务端确认本次连接检查通过。没有发送业务消息。'; queueRefresh('channels', 'timeline'); }
      catch (error) { result.className = 'test-result form-error'; result.textContent = errorMessage(error); throw error; }
    }, { confirm: '检查已保存的连接' });
  }
  function renderBindings() {
    const root = $('#binding-list'); if (state.errors.bindings) { root.replaceChildren(unavailable('bindings')); return; }
    const entries = [...state.bindings.pending.map(item => ({ ...item, pending: true })), ...state.bindings.bindings.map(item => ({ ...item, pending: false }))];
    if (!entries.length) { root.replaceChildren(empty(state.loaded.bindings ? '尚未绑定所有者' : '正在读取绑定', '先保存并启用对应渠道（建议试运行），再创建短期绑定码。在机器人私聊发送 /bind CODE，然后回到网页核对候选用户。', 'link')); return; }
    root.replaceChildren(...entries.slice(0, 100).map(item => {
      const row = node('div', 'binding-row'); const details = node('div'); const expires = item.expires_at ? new Date(item.expires_at).getTime() : null;
      const expired = expires !== null && Number.isFinite(expires) && expires <= Date.now();
      details.append(node('h3', '', `${channelName(item.channel)} · ${item.pending ? expired ? '绑定码已过期' : item.user_id ? '已收到候选用户，待网页确认' : '等待机器人收到绑定码' : '已确认绑定'}`));
      if (item.user_id) details.append(node('p', '', `用户 ID：${text(item.user_id, 150)}${item.bot_id ? ` · 机器人 ID：${text(item.bot_id, 150)}` : ''}`));
      if (item.pending && !expired && item.code) { const p = node('p'); p.append(node('span', '', '在机器人私聊发送 '), node('code', 'mono', `/bind ${item.code}`)); details.append(p); }
      details.append(node('p', '', item.pending ? `有效期至 ${date(item.expires_at)}` : `确认于 ${date(item.created_at)}`));
      const actions = node('div', 'inline-actions');
      if (item.pending) { const approve = button('核对并绑定', 'primary small', 'check', () => approveBinding(item)); approve.disabled = !item.user_id || expired; actions.append(approve); }
      if (item.pending) details.append(node('p', '', '未确认的请求不会授予权限，到期自动作废。'));
      else actions.append(button('解绑', 'secondary small', 'close', () => removeBinding(item)));
      row.append(details, actions); return row;
    }));
  }
  function createBinding() {
    const channel = $('#binding-channel').value;
    confirmAction(`创建 ${channelName(channel)} 绑定码？`, '绑定码用于匹配机器人私聊用户，不等同于管理令牌。请仅发给你自己的机器人，随后必须回到网页核对候选用户 ID 并批准。', async () => {
      const result = await api('/api/bindings', { method: 'POST', body: { channel } });
      if (!result.id) throw new Error('服务未返回绑定请求 ID。'); await loadData('bindings'); toast('绑定码已创建，请查看下方待绑定卡片。');
    }, { confirm: '创建短期绑定码' });
  }
  function approveBinding(item) {
    confirmAction('将这位用户绑定为所有者？', '绑定后，这个 QQ 私聊用户可以提交其授权范围内的任务。具体敏感操作仍须在网页审批。请确认用户与机器人 ID 确实属于你的预期。', async () => {
      await api(`/api/bindings/${encodeURIComponent(item.id)}/approve`, { method: 'POST', body: {} }); await loadData('bindings'); toast('所有者绑定已确认。');
    }, { confirm: '确认身份并绑定', danger: true, preview: `渠道：${channelName(item.channel)}\n用户 ID：${item.user_id}\n机器人 ID：${item.bot_id || '服务端未返回，请核对预期机器人配置'}` });
  }
  function removeBinding(item) {
    confirmAction(item.pending ? '撤销这次绑定请求？' : '移除所有者绑定？', item.pending ? '此绑定码将不能再完成授权。需要时可以重新创建。' : '对应 QQ 用户将失去此绑定提供的私聊授权。是否有运行中的任务会由后端再次检查。', async () => {
      await api(`/api/bindings/${encodeURIComponent(item.id)}`, { method: 'DELETE', body: {} }); await loadData('bindings'); toast(item.pending ? '绑定请求已撤销。' : '绑定已移除。');
    }, { confirm: item.pending ? '撤销请求' : '移除绑定', danger: true, preview: `${channelName(item.channel)} · ${item.user_id || '尚无候选用户'}` });
  }
  function renderNotifications() {
    const root = $('#notifications-list'); if (state.errors.notifications) { root.replaceChildren(unavailable('notifications')); return; }
    if (!state.notifications.length) { root.replaceChildren(empty(state.loaded.notifications ? '没有通知记录' : '正在读取通知', state.config?.watch?.enabled ? '满足已保存的阈值与频率条件后，真实通知会在此出现。' : '自动值守当前关闭。需要时配置阈值、接收者与免打扰后再保存启用。', 'bell')); return; }
    root.replaceChildren(...state.notifications.slice(0, 60).map(item => {
      const row = node('div', 'notification-row'); const body = node('div'); body.append(node('p', '', text(item.message || '服务端未提供通知说明', 1800)));
      const metric = ({ cpu: 'CPU', memory: '内存', disk: '磁盘' })[item.metric] || item.metric || '未知指标';
      body.append(node('small', '', `${metric}${number(item.value) !== null ? ` · 当前 ${Number(item.value).toFixed(1)}` : ''}${number(item.threshold) !== null ? ` / 阈值 ${Number(item.threshold).toFixed(1)}` : ''} · ${date(item.created_at, true)}`));
      row.append(icon('bell'), body, statusPill(item.state)); return row;
    }));
  }
  function rotateToken() {
    const input = field('当前访问令牌', null, '', { type: 'password', required: true, maxLength: 512 });
    const ack = node('label', 'check-row'); const check = node('input'); check.type = 'checkbox'; check.required = true; ack.append(check, node('span', '', '我理解所有登录会话都会失效，并已准备好保存新令牌。'));
    openDialog({ title: '更换访问令牌', eyebrow: 'ROTATE ACCESS', body: [notice('新令牌只显示一次。旧令牌立即失效，其他网页会话需重新登录。', true), input, ack], confirm: '使旧令牌失效并更换', danger: true, onSubmit: async () => {
      let token = $('input', input).value.trim(); $('input', input).value = '';
      try { const result = await api('/api/token/rotate', { method: 'POST', body: { confirm_token: token } }); token = ''; closeDialog(true); showToken(result.token); }
      finally { token = ''; }
    } });
  }
  function logout() {
    const dirty = ['settings', 'channels', 'watch'].some(key => $(`#${key}-config-form`).dataset.dirty === '1');
    confirmAction('退出工作台？', `${dirty ? '未保存的配置更改将丢失。\n' : ''}退出会清除当前浏览器中的临时输入和会话数据，但不会取消已经提交到服务端的任务。`, async () => {
      await api('/api/logout', { method: 'POST', body: {} }); showAuth('login'); toast('已退出登录。');
    }, { confirm: '退出登录' });
  }

  // Static listeners are installed once. Live refresh only replaces data regions,
  // never the composer, an edited configuration form, or the active dialog.
  $('#boot-retry').addEventListener('click', bootstrap);
  $('#copy-setup-command').addEventListener('click', () => runAction(async () => {
    const input = $('#setup-command');
    try { if (!navigator.clipboard || !window.isSecureContext) throw new Error(); await navigator.clipboard.writeText(input.value); toast('命令已复制。运行前请替换为服务使用的同一数据目录。'); }
    catch { input.focus(); input.select(); toast('命令已选中，请手动复制并替换数据目录。'); }
  }));
  $('#login-form').addEventListener('submit', login); $('#unlock-form').addEventListener('submit', unlock);
  $('#setup-form').addEventListener('submit', event => event.preventDefault());
  $('#setup-next').addEventListener('click', () => runAction(nextSetup));
  $('#setup-back').addEventListener('click', () => { if (state.setup && state.setup.step > 0) { captureSetup(false); state.setup.step--; renderSetup(); } });
  $('#setup-skip').addEventListener('click', () => { if (!state.setup) return; state.setup.config.model = { base_url: '', name: '' }; delete state.setup.secrets.model_key; clearSecrets($('#setup-form')); state.setup.step = 2; renderSetup(); });
  $('#token-saved').addEventListener('change', event => { $('#token-login').disabled = !event.currentTarget.checked; });
  $('#token-login').addEventListener('click', () => runAction(tokenLogin)); $('#copy-token').addEventListener('click', () => runAction(copyToken));
  $('#reveal-token').addEventListener('click', () => { const input = $('#new-token'); input.type = input.type === 'password' ? 'text' : 'password'; $('#reveal-token').textContent = input.type === 'password' ? '显示' : '隐藏'; });
  $('#dialog-form').addEventListener('submit', submitDialog); $('#dialog-close').addEventListener('click', () => closeDialog()); $('#dialog-cancel').addEventListener('click', () => closeDialog());
  $('#action-dialog').addEventListener('cancel', event => { event.preventDefault(); closeDialog(); });
  $$('.nav-item').forEach(b => b.addEventListener('click', () => navigate(b.dataset.view)));
  $$('[data-navigate]').forEach(b => b.addEventListener('click', () => navigate(b.dataset.navigate)));
  $$('[data-prompt]').forEach(b => b.addEventListener('click', () => setPrompt(b.dataset.prompt)));
  $('#nav-toggle').addEventListener('click', () => { const open = !$('#sidebar').classList.contains('open'); $('#sidebar').classList.toggle('open', open); $('#nav-backdrop').hidden = !open; $('#nav-toggle').setAttribute('aria-expanded', String(open)); });
  $('#nav-backdrop').addEventListener('click', closeNav);
  $('#context-toggle').addEventListener('click', () => {
    state.contextOpen = !state.contextOpen; closeNav(); renderContextLayout();
    if (state.contextOpen) $('#context-close').focus();
  });
  $('#context-close').addEventListener('click', () => closeContext(true));
  $('#context-backdrop').addEventListener('click', () => closeContext(true));
  wideContext.addEventListener('change', () => { state.contextOpen = false; renderContextLayout(); });
  document.addEventListener('keydown', event => {
    if (event.key === 'Escape' && state.contextOpen && !$('#action-dialog').open) { event.preventDefault(); closeContext(true); }
  });
  window.addEventListener('hashchange', () => { if (state.authenticated && VIEWS[location.hash.slice(1)]) navigate(location.hash.slice(1), false); });
  $('#refresh-all').addEventListener('click', () => runAction(async () => { await loadData('config'); await refreshAll(); }));
  $('#logout').addEventListener('click', logout); $('#new-session').addEventListener('click', newSession); $('#delete-session').addEventListener('click', deleteSession);
  $('#message-form').addEventListener('submit', sendTask);
  $('#message-input').maxLength = 12000;
  $('#message-input').addEventListener('input', event => { state.drafts.set(state.selectedSession, event.currentTarget.value); });
  $('#message-input').addEventListener('keydown', event => { if ((event.ctrlKey || event.metaKey) && event.key === 'Enter' && !event.isComposing) { event.preventDefault(); $('#message-form').requestSubmit(); } });
  $('#memory-search').addEventListener('input', () => { window.clearTimeout(state.memorySearchTimer); state.memorySearchEpoch++; state.memorySearchTimer = window.setTimeout(searchMemories, 350); });
  $('#memory-filter').addEventListener('change', renderMemories); $('#add-memory').addEventListener('click', () => editMemory());
  $('#timeline-filter').addEventListener('change', renderTimeline); $('#create-binding').addEventListener('click', createBinding); $('#rotate-token').addEventListener('click', rotateToken);
  document.addEventListener('visibilitychange', () => { if (!document.hidden && state.authenticated) { queueRefresh('config', 'metrics', 'tasks', 'approvals', 'timeline', 'conversation', 'bindings'); } });
  window.addEventListener('beforeunload', event => {
    const dirty = state.phase === 'token' || state.phase === 'setup' || ['settings', 'channels', 'watch'].some(key => $(`#${key}-config-form`).dataset.dirty === '1');
    if (dirty) { event.preventDefault(); event.returnValue = ''; }
  });
  window.addEventListener('pagehide', () => { stopLive(); clearSecrets(); $('#new-token').value = ''; if (state.setup) state.setup.secrets = {}; });
  window.addEventListener('pageshow', event => { if (event.persisted) { discardAuthenticatedState(); bootstrap(); } });
  bootstrap();
})();
