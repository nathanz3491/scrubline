#!/usr/bin/env node
// Hands straight over to the real binary: same arguments, same streams, same
// exit code. Nothing about the tool's behaviour lives here.
//
// The binary is fetched on first use if it is not already present, because npm
// blocks install scripts by default and `npx` never runs them.
const fs = require('fs');
const { spawnSync } = require('child_process');
const { ensureBinary, binaryPath } = require('../scripts/install.js');

async function main() {
  let binary = binaryPath();
  if (!fs.existsSync(binary)) {
    try {
      binary = await ensureBinary();
    } catch (err) {
      console.error(`scrubline: ${err.message}`);
      console.error('install the binary directly instead: https://github.com/nathanz3491/scrubline#install');
      process.exit(1);
    }
  }
  const result = spawnSync(binary, process.argv.slice(2), { stdio: 'inherit' });
  if (result.error) {
    console.error(`scrubline: ${result.error.message}`);
    process.exit(1);
  }
  process.exit(result.status === null ? 1 : result.status);
}

main();
