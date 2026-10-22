// Run with: node --test tests/markdown.test.cjs
const {test} = require('node:test');
const assert = require('node:assert/strict');
const markdown = require('../web/static/vendor/markdown-it.min.js')({html: false, linkify: true, typographer: false});
markdown.linkify.set({fuzzyLink: true});

test('renders headings, lists, emphasis, code, quotes, and tables', () => {
  const html = markdown.render('# Heading\n\n- **bold** and *italic* with `code`\n\n> quote\n\n```js\nconst n = 1;\n```\n\n| A | B |\n| --- | --- |\n| 1 | 2 |');
  for (const expected of ['<h1>Heading</h1>', '<ul>', '<strong>bold</strong>', '<em>italic</em>', '<code>code</code>', '<blockquote>', '<pre><code class="language-js">', '<table>']) {
    assert.ok(html.includes(expected), expected);
  }
});

test('escapes raw HTML and rejects executable links', () => {
  const html = markdown.render('<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[bad](javascript:alert%281%29)\n\n[encoded](jav&#x61;script:alert%281%29)\n\n[good](https://example.com)');
  assert.ok(!html.includes('<script>'));
  assert.ok(!html.includes('<img'));
  assert.ok(!html.includes('href="javascript:'));
  assert.ok(html.includes('href="https://example.com"'));
});

test('linkifies bare URLs without including trailing punctuation', () => {
  const html = markdown.render('Visit https://example.com/path?q=1 and www.example.org.');
  assert.ok(html.includes('href="https://example.com/path?q=1"'));
  assert.ok(html.includes('href="http://www.example.org"'));
  assert.ok(html.includes('www.example.org</a>.'));
});

test('does not linkify URLs inside code or executable schemes', () => {
  const html = markdown.render('`https://example.com`\n\n```text\nhttps://example.org\n```\n\njavascript:alert(1)');
  assert.ok(!html.includes('<a '));
});

test('incomplete streaming fences remain escaped code', () => {
  const html = markdown.render('## Working\n\n```html\n<script>');
  assert.ok(html.includes('<h2>Working</h2>'));
  assert.ok(html.includes('&lt;script&gt;'));
});
