'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

function extract(name) {
  const starts = [source.indexOf(`function ${name}(`), source.indexOf(`async function ${name}(`)].filter(index => index >= 0);
  assert.ok(starts.length);
  const start = Math.min(...starts), open = source.indexOf('{', start);
  let depth = 0;
  for (let i = open; i < source.length; i++) {
    if (source[i] === '{') depth++;
    if (source[i] === '}' && --depth === 0) return source.slice(start, i + 1);
  }
  throw new Error('Missing function end');
}

function harness(readiness = { ok: false, stage: 'family-credential-required' }) {
  const elements = new Map(), reads = [], writes = [], notices = [];
  let read = async () => readiness, mutate = async () => {};
  const node = id => {
    if (!elements.has(id)) elements.set(id, { value: '', textContent: '', open: false, focus() {}, showModal() { this.open = true; }, close() { this.open = false; } });
    return elements.get(id);
  };
  node('familyOrganizer').value = 'organizer-a';
  node('familyOrganizer').selectedOptions = [{ textContent: '测试母号' }];
  const context = vm.createContext({
    selectedAccount: 'target-a', current: { login_account: 'child@example.test' }, familyAddContext: null, busy: false,
    $: node, confirm: () => true, render() {}, notice: (...args) => notices.push(args),
    api: async (route, payload) => { reads.push({ route, ...payload }); return read(); },
    action: async (action, payload) => { writes.push({ action, payload: { ...payload }, reference: payload }); return mutate(); },
  });
  for (const name of ['clearFamilyPassword', 'familyAddSelectionMatches', 'requestFamilyAdd', 'submitFamilyPassword']) vm.runInContext(extract(name), context);
  return { context, node, reads, writes, notices, setRead: fn => { read = fn; }, setMutation: fn => { mutate = fn; }, submit: () => context.submitFamilyPassword({ preventDefault() {} }) };
}

test('resumed online account asks for child password without reconnecting or sending SMS', async () => {
  const ui = harness();
  await ui.context.requestFamilyAdd();
  assert.equal(ui.node('familyPasswordDialog').open, true);
  assert.equal(ui.node('familyPasswordAccount').value, 'child@example.test');
  assert.match(ui.node('familyPasswordOrganizer').textContent, /测试母号/);
  assert.deepEqual(ui.reads, [{ route: 'action', action: 'family-add-readiness' }]);
  assert.equal(ui.writes.length, 0);
});

test('an available saved or in-memory password uses the existing family action', async () => {
  const ui = harness({ ok: true, stage: 'family-add-ready' });
  await ui.context.requestFamilyAdd();
  assert.equal(ui.node('familyPasswordDialog').open, false);
  assert.deepEqual(ui.writes[0].payload, { code: 'organizer-a' });
  assert.equal(ui.writes[0].action, 'family-add-self');
});

test('one-shot submit binds both accounts and clears the input and payload on failure', async () => {
  const ui = harness();
  await ui.context.requestFamilyAdd();
  ui.node('familyPassword').value = 'fixture-one-shot';
  ui.setMutation(async () => { throw new Error('fixture transport failure'); });
  await assert.rejects(ui.submit(), /fixture transport failure/);
  assert.deepEqual(ui.writes[0].payload, { code: 'organizer-a', target_apple_id: 'child@example.test', target_password: 'fixture-one-shot' });
  assert.equal(ui.writes[0].reference.target_password, '');
  assert.equal(ui.node('familyPassword').value, '');
  assert.equal(ui.node('familyPasswordDialog').open, false);
  assert.equal(ui.context.familyAddContext, null);
});

test('cancel clears the password without a family mutation', async () => {
  const ui = harness();
  await ui.context.requestFamilyAdd();
  ui.node('familyPassword').value = 'fixture-cancelled';
  ui.context.clearFamilyPassword();
  assert.equal(ui.node('familyPassword').value, '');
  assert.equal(ui.context.familyAddContext, null);
  assert.equal(ui.writes.length, 0);
});

test('changed target or organizer invalidates a pending password submission', async () => {
  for (const kind of ['target', 'organizer', 'identity']) {
    const ui = harness();
    await ui.context.requestFamilyAdd();
    ui.node('familyPassword').value = 'fixture-secret';
    if (kind === 'target') ui.context.selectedAccount = 'target-b';
    if (kind === 'organizer') ui.node('familyOrganizer').value = 'organizer-b';
    if (kind === 'identity') ui.context.current.login_account = 'other@example.test';
    await ui.submit();
    assert.equal(ui.writes.length, 0);
    assert.equal(ui.node('familyPassword').value, '');
    assert.equal(ui.context.familyAddContext, null);
  }
});

test('late readiness and real offline failures do not open an unrelated password dialog', async () => {
  const ui = harness();
  let resolve;
  ui.setRead(() => new Promise(done => { resolve = done; }));
  const waiting = ui.context.requestFamilyAdd();
  ui.context.selectedAccount = 'target-b';
  resolve({ ok: false, stage: 'family-credential-required' });
  await waiting;
  assert.equal(ui.node('familyPasswordDialog').open, false);
  const offline = harness({ ok: false, stage: 'family-worker-unready', message: '后台未在线' });
  await offline.context.requestFamilyAdd();
  assert.equal(offline.node('familyPasswordDialog').open, false);
  assert.equal(offline.writes.length, 0);
  assert.match(offline.notices[0][0], /未在线/);
});
