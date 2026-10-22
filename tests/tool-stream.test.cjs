// Run with: node --test tests/tool-stream.test.cjs
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require.resolve('../web/static/session.js'), 'utf8');

// Run the actual production handlers with a minimal DOM double.
function toolContext() {
  const nodes = new Map();
  const appended = [];
  const document = {
    getElementById: id => nodes.get(id),
    createTextNode: text => ({nodeType: 3, textContent: text}),
  };
  const context = vm.createContext({
    document,
    transcript: {append: node => appended.push(node)},
    frag: html => ({html, firstElementChild: {matches: selector => selector === 'details', open: false}}),
  });
  const handlers = source.slice(source.indexOf('  const handlers = {'), source.indexOf('  function apply(events)'));
  vm.runInContext(handlers + '\nglobalThis.handlers = handlers;', context);
  return {nodes, appended, handlers: context.handlers};
}

function toolNode(open = true) {
  const chunks = [];
  const pre = {textContent: '', append: node => chunks.push(node)};
  return {
    open,
    chunks,
    pre,
    matches: selector => selector === 'details',
    querySelector: selector => selector === 'pre' ? pre : null,
    replaceWith(replacement) { this.replacement = replacement; },
    remove() { this.removed = true; },
  };
}

test('tool chunks append as literal text, independently per tool', () => {
  const {nodes, handlers} = toolContext();
  const one = toolNode();
  const two = toolNode();
  nodes.set('tool-one', one);
  nodes.set('tool-two', two);
  handlers.tool_delta({id: 'tool-one', s: '<script>alert(1)</script>\n'});
  handlers.tool_delta({id: 'tool-two', s: 'other\n'});
  handlers.tool_delta({id: 'tool-one', s: 'second chunk\n'});
  assert.equal(one.chunks.length, 2);
  assert.equal(one.chunks[0].nodeType, 3);
  assert.equal(one.chunks[0].textContent, '<script>alert(1)</script>\n');
  assert.equal(one.chunks[1].textContent, 'second chunk\n');
  assert.equal(two.chunks[0].textContent, 'other\n');
});

test('snapshot revisions clear stale output and tolerate missing placeholders', () => {
  const {nodes, handlers} = toolContext();
  const tool = toolNode();
  tool.pre.textContent = 'old output';
  nodes.set('tool-one', tool);
  handlers.tool_reset({id: 'tool-one'});
  assert.equal(tool.pre.textContent, '');
  assert.doesNotThrow(() => handlers.tool_delta({id: 'missing', s: 'ignored'}));
  assert.doesNotThrow(() => handlers.tool_reset({id: 'missing'}));
});

test('completed results replace live output once and preserve expansion', () => {
  for (const open of [true, false]) {
    const {nodes, appended, handlers} = toolContext();
    const tool = toolNode(open);
    nodes.set('tool-one', tool);
    handlers.append({id: 'tool-one', html: '<details>completed</details>'});
    assert.equal(appended.length, 0);
    assert.equal(tool.replacement.html, '<details>completed</details>');
    assert.equal(tool.replacement.firstElementChild.open, open);
  }
});

test('append still handles assistant replacement, hidden messages and new output', () => {
  const {nodes, appended, handlers} = toolContext();
  const assistant = toolNode();
  assistant.matches = () => false;
  nodes.set('live', assistant);
  handlers.append({id: 'live', html: '<article>done</article>'});
  assert.equal(assistant.replacement.html, '<article>done</article>');
  handlers.append({id: 'live'});
  assert.equal(assistant.removed, true);
  handlers.append({id: 'tool-new', html: '<details>running</details>'});
  assert.equal(appended.length, 1);
  assert.equal(appended[0].html, '<details>running</details>');
});
