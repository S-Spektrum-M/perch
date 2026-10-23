const {test} = require('node:test');
const assert = require('node:assert/strict');
const {fuzzyScore} = require('../web/static/new.js');

test('project fuzzy search matches multiple abbreviated tokens', () => {
  assert.ok(Number.isFinite(fuzzyScore('cat tui', 'catalyst-tui', '/projects/catalyst-tui')));
  assert.equal(fuzzyScore('cat tui', 'perch', '/projects/perch'), -Infinity);
});
test('project search favors names, contiguous matches, and ignores case', () => {
  assert.ok(fuzzyScore('perch', 'perch', '/projects/perch') > fuzzyScore('perch', 'other', '/projects/perch/other'));
  assert.ok(fuzzyScore('cat', 'cat', '/cat') > fuzzyScore('cat', 'c_a_t', '/c_a_t'));
  assert.equal(fuzzyScore('CAT TUI', 'Catalyst-TUI', '/Projects/Catalyst-TUI'), fuzzyScore('cat tui', 'catalyst-tui', '/projects/catalyst-tui'));
});
