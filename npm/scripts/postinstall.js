#!/usr/bin/env node
// Prefetch the binary at install time when npm allows install scripts.
//
// A failure here is not fatal: npm 11 blocks install scripts by default, and
// the launcher downloads on first use anyway. Warn and move on rather than
// failing somebody's `npm install`.
const { ensureBinary } = require('./install.js');

ensureBinary().catch((err) => {
  console.error(`scrubline: could not prefetch the binary (${err.message})`);
  console.error('scrubline: it will be downloaded on first use instead');
});
