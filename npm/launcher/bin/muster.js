#!/usr/bin/env node
// Launcher for the muster binary (#243).
//
// This package carries no binary. npm installs exactly one of the sibling
// @futurelastic/muster-<os>-<cpu> packages (their `os`/`cpu` fields do the
// choosing, via optionalDependencies), and this file finds it and runs it.
// Nothing is downloaded at install time, so `--ignore-scripts` changes nothing.
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');

const SUPPORTED = ['darwin-arm64', 'darwin-x64', 'linux-x64', 'linux-arm64'];

function fail(message) {
  process.stderr.write(`muster: ${message}\n`);
  process.exit(1);
}

// resolveBinary returns the absolute path of the platform package's binary, or
// throws an Error whose message says what to do. Exported through `require.main`
// so the test can drive it without spawning anything.
function resolveBinary(platform, arch, resolve, ownVersion) {
  const key = `${platform}-${arch}`;
  if (!SUPPORTED.includes(key)) {
    throw new Error(
      `no prebuilt binary for ${key} (supported: ${SUPPORTED.join(', ')}). ` +
        'Build from source: https://github.com/futurelastic/muster#install'
    );
  }
  const pkg = `@futurelastic/muster-${key}`;
  let manifest;
  try {
    manifest = resolve(`${pkg}/package.json`);
  } catch {
    throw new Error(
      `${pkg} is not installed. It is an optional dependency of @futurelastic/muster, ` +
        'so an install that skipped optional dependencies (--omit=optional) or copied ' +
        'node_modules between platforms leaves it out. Reinstall on this machine.'
    );
  }
  const theirs = JSON.parse(fs.readFileSync(manifest, 'utf8')).version;
  if (theirs !== ownVersion) {
    // The versions are published together and must stay together: a launcher
    // running a different build than the one it was released with is a
    // support problem nobody can see from `muster --version` alone.
    throw new Error(
      `version mismatch: @futurelastic/muster is ${ownVersion} but ${pkg} is ${theirs}. ` +
        'Reinstall so both come from the same release.'
    );
  }
  return path.join(path.dirname(manifest), 'bin', 'muster');
}

function run() {
  const own = require('../package.json').version;
  let bin;
  try {
    bin = resolveBinary(process.platform, process.arch, require.resolve, own);
  } catch (err) {
    fail(err.message);
  }
  // A tarball extracted without the executable bit (some mirrors do this)
  // would otherwise fail with an opaque EACCES.
  try {
    fs.accessSync(bin, fs.constants.X_OK);
  } catch {
    try {
      fs.chmodSync(bin, 0o755);
    } catch (err) {
      fail(`${bin} is not executable and could not be made so: ${err.message}`);
    }
  }

  const child = spawn(bin, process.argv.slice(2), { stdio: 'inherit' });
  // The service handles these itself (a graceful stop); pass them on rather
  // than dying first and orphaning it.
  for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    process.on(sig, () => child.kill(sig));
  }
  child.on('error', (err) => fail(`could not start ${bin}: ${err.message}`));
  child.on('exit', (code, signal) => {
    if (signal) {
      process.removeAllListeners(signal);
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code === null ? 1 : code);
  });
}

if (require.main === module) {
  run();
} else {
  module.exports = { resolveBinary, SUPPORTED };
}
