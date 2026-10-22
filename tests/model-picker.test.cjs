// Run with: node --test tests/model-picker.test.cjs
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require.resolve('../web/static/session.js'), 'utf8');

// Exercise the production event handlers without requiring a browser DOM.
function pickerContext() {
  const requests = [];
  const listeners = {};
  const context = vm.createContext({
    URLSearchParams,
    pickerBusy: false,
    picker: {addEventListener: (event, handler) => { listeners[event] = handler; }},
    updatePicker: body => requests.push(body),
  });
  const select = source.slice(source.indexOf('  function selectModel('), source.indexOf('  function colorEffortSlider('));
  const change = source.slice(source.indexOf('  picker.addEventListener("change"'), source.indexOf('  // A response spans'));
  vm.runInContext(select + change, context);
  return {context, requests, listeners};
}

async function fields(body) {
  // Fetch must infer the encoding that Go's Request.ParseForm accepts.
  const request = new Request('http://localhost/model', {method: 'POST', body});
  assert.match(request.headers.get('content-type'), /^application\/x-www-form-urlencoded\b/);
  return new URLSearchParams(await request.text());
}

test('model selection posts URL-encoded setting, provider, and model ID', async () => {
  const {context, requests} = pickerContext();
  context.selectModel({dataset: {provider: 'test-provider', modelId: 'model/+ special'}});
  assert.equal(requests.length, 1);
  const form = await fields(requests[0]);
  assert.equal(form.get('setting'), 'model');
  assert.equal(form.get('provider'), 'test-provider');
  assert.equal(form.get('modelId'), 'model/+ special');
});

test('checking make default saves the current selection; unchecking does not save', () => {
  const {listeners, requests} = pickerContext();
  const target = {matches: selector => selector === '[data-make-default]', checked: true};
  listeners.change({target});
  assert.equal(requests.length, 1);
  assert.equal(requests[0].get('setting'), 'default');
  target.checked = false;
  listeners.change({target});
  assert.equal(requests.length, 1);
});

test('effort selection posts URL-encoded setting and level', async () => {
  const {listeners, requests} = pickerContext();
  listeners.change({target: {
    matches: selector => selector === '[data-effort-slider]',
    value: '1',
    closest: () => ({querySelectorAll: () => [
      {dataset: {effortLevel: 'low'}},
      {dataset: {effortLevel: 'high'}},
    ]}),
  }});
  assert.equal(requests.length, 1);
  const form = await fields(requests[0]);
  assert.equal(form.get('setting'), 'effort');
  assert.equal(form.get('level'), 'high');
});
