// Downloads the scrubline binary that matches this machine.
//
// This package is a shim, not a second implementation: it fetches the same
// release archive install.sh does, verifies it against the published
// checksums, and unpacks the single binary. No dependencies, because a tool
// whose pitch is "no runtime to install first" should not arrive with a
// dependency tree.
//
// It runs from two places. postinstall prefetches the binary when npm allows
// install scripts to run; npm 11 blocks them by default, so the launcher also
// calls this on first use when the binary is missing. That second path is the
// one `npx scrubline` actually takes.
//
// SCRUBLINE_BASE_URL overrides where archives come from, which is how this is
// tested against a local `goreleaser --snapshot` build.
const fs = require('fs');
const os = require('os');
const path = require('path');
const https = require('https');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const REPO = 'nathanz3491/scrubline';
const pkg = require('../package.json');
const version = process.env.SCRUBLINE_VERSION || `v${pkg.version}`;
const vendorDir = path.join(__dirname, '..', 'vendor');

function platform() {
  const goos = { darwin: 'darwin', linux: 'linux', win32: 'windows' }[process.platform];
  const goarch = { x64: 'amd64', arm64: 'arm64' }[process.arch];
  if (!goos || !goarch) {
    throw new Error(`unsupported platform ${process.platform}/${process.arch}`);
  }
  if (goos === 'windows' && goarch === 'arm64') {
    throw new Error('windows/arm64 is not published');
  }
  return { goos, goarch, ext: goos === 'windows' ? 'zip' : 'tar.gz' };
}

function fetch(url, redirects = 0) {
  if (url.startsWith('file://')) {
    return Promise.resolve(fs.readFileSync(decodeURIComponent(url.slice('file://'.length))));
  }
  return new Promise((resolve, reject) => {
    if (redirects > 5) return reject(new Error(`too many redirects for ${url}`));
    https
      .get(url, { headers: { 'user-agent': 'scrubline-npm' } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          res.resume();
          return resolve(fetch(new URL(res.headers.location, url).toString(), redirects + 1));
        }
        if (res.statusCode !== 200) {
          res.resume();
          return reject(new Error(`GET ${url} returned ${res.statusCode}`));
        }
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => resolve(Buffer.concat(chunks)));
      })
      .on('error', reject);
  });
}

function verify(archive, checksums, name) {
  const line = checksums
    .toString('utf8')
    .split('\n')
    .find((l) => l.trim().endsWith(` ${name}`) || l.trim().endsWith(`  ${name}`));
  if (!line) throw new Error(`no checksum published for ${name}`);
  const want = line.trim().split(/\s+/)[0];
  const got = crypto.createHash('sha256').update(archive).digest('hex');
  if (want !== got) throw new Error(`checksum mismatch for ${name}: expected ${want}, got ${got}`);
}

function unpack(archivePath, ext, dest) {
  fs.mkdirSync(dest, { recursive: true });
  if (ext === 'zip') {
    // tar.exe on Windows 10+ reads zip; PowerShell is the fallback.
    try {
      execFileSync('tar', ['-xf', archivePath, '-C', dest], { stdio: 'ignore' });
    } catch {
      execFileSync('powershell', ['-NoProfile', '-Command',
        `Expand-Archive -Path '${archivePath}' -DestinationPath '${dest}' -Force`], { stdio: 'ignore' });
    }
  } else {
    execFileSync('tar', ['-xzf', archivePath, '-C', dest], { stdio: 'ignore' });
  }
}

function binaryPath() {
  return path.join(vendorDir, process.platform === 'win32' ? 'scrubline.exe' : 'scrubline');
}

async function ensureBinary({ quiet = false } = {}) {
  if (fs.existsSync(binaryPath())) return binaryPath();
  const { goos, goarch, ext } = platform();
  const name = `scrubline_${goos}_${goarch}.${ext}`;
  const base = process.env.SCRUBLINE_BASE_URL || `https://github.com/${REPO}/releases/download/${version}`;

  const [archive, checksums] = await Promise.all([fetch(`${base}/${name}`), fetch(`${base}/checksums.txt`)]);
  verify(archive, checksums, name);

  fs.mkdirSync(vendorDir, { recursive: true });
  const tmp = path.join(os.tmpdir(), `scrubline-${process.pid}-${name}`);
  fs.writeFileSync(tmp, archive);
  try {
    unpack(tmp, ext, vendorDir);
  } finally {
    fs.rmSync(tmp, { force: true });
  }

  const binary = binaryPath();
  if (!fs.existsSync(binary)) throw new Error('the archive did not contain a scrubline binary');
  fs.chmodSync(binary, 0o755);
  if (!quiet) console.error(`scrubline ${version} installed for ${goos}/${goarch}`);
  return binary;
}

module.exports = { ensureBinary, binaryPath };
