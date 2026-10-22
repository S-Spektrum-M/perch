# Vendored browser dependencies

- `markdown-it.min.js`: markdown-it **15.0.3**, from `dist/browser/markdown-it.umd.min.js` in the npm package. MIT license: `markdown-it.LICENSE`.

Served locally so rendering does not require a CDN. Raw HTML is disabled, unsafe link protocols are rejected by markdown-it, and image rendering is replaced with escaped placeholder text in `session.js` to avoid automatic remote requests.
