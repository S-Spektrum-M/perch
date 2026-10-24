const {test} = require('node:test');
const assert = require('node:assert/strict');
const {matches, label, adjacent} = require('../web/static/keys.js');

const key = (k, mods = {}) => ({key: k, ctrlKey: false, metaKey: false, altKey: false, shiftKey: false, ...mods});

test('Mod is Ctrl off Apple platforms and Cmd on them', () => {
  assert.ok(matches('Mod+K', key('k', {ctrlKey: true}), false));
  assert.ok(!matches('Mod+K', key('k', {metaKey: true}), false));
  assert.ok(matches('Mod+K', key('k', {metaKey: true}), true));
  assert.ok(!matches('Mod+K', key('k', {ctrlKey: true}), true));
});

test('modifiers must match exactly and letters ignore case', () => {
  assert.ok(!matches('Mod+K', key('K', {ctrlKey: true, shiftKey: true}), false));
  assert.ok(matches('Mod+Shift+O', key('O', {ctrlKey: true, shiftKey: true}), false));
  assert.ok(!matches('Mod+Shift+O', key('o', {ctrlKey: true}), false));
  assert.ok(matches('Alt+ArrowDown', key('ArrowDown', {altKey: true}), false));
  assert.ok(!matches('Alt+ArrowDown', key('ArrowDown'), false));
});

test('labels use platform conventions', () => {
  assert.equal(label('Mod+Shift+O', false), 'Ctrl+Shift+O');
  assert.equal(label('Mod+Shift+O', true), '⌘⇧O');
  assert.equal(label('Alt+ArrowUp', true), '⌥↑');
  assert.equal(label('Alt+ArrowDown', false), 'Alt+↓');
});

test('adjacent rotates through sessions', () => {
  const s = ['/s/a', '/s/b', '/s/c'];
  assert.equal(adjacent(s, '/s/b', 1), '/s/c');
  assert.equal(adjacent(s, '/s/b', -1), '/s/a');
  assert.equal(adjacent(s, '/s/c', 1), '/s/a');
  assert.equal(adjacent(s, '/s/a', -1), '/s/c');
  assert.equal(adjacent(s, null, 1), '/s/a');
  assert.equal(adjacent(s, null, -1), '/s/c');
  assert.equal(adjacent([], null, 1), null);
});
