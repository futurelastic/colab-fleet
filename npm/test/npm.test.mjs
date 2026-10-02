// Tests for the npm packaging (#243): the launcher's resolution rules and the
// allowlist of every assembled package. Run: node --test npm/test/npm.test.mjs
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { assemble, PLATFORMS, versionFromTag, distTag } from '../../scripts/assemble-npm.mjs';

const require = createRequire(import.meta.url);
const { resolveBinary } = require('../launcher/bin/muster.js');
const repo = join(fileURLToPath(import.meta.url), '../../..');

function fakeBinaries(tag) {
  const dir = mkdtempSync(join(tmpdir(), 'muster-bin-'));
  for (const p of PLATFORMS) writeFileSync(join(dir, `muster-${tag}-${p.goos}-${p.goarch}`), '#!/bin/sh\necho muster\n');
  return dir;
}

test('version comes from the tag; prereleases go to next', () => {
  assert.equal(versionFromTag('v1.2.3'), '1.2.3');
  assert.equal(versionFromTag('v1.2.3-rc.1'), '1.2.3-rc.1');
  assert.throws(() => versionFromTag('1.2.3'));
  assert.throws(() => versionFromTag('v1.2'));
  assert.equal(distTag('1.2.3'), 'latest');
  assert.equal(distTag('1.2.3-rc.1'), 'next');
});

test('every package packs exactly its allowlist', () => {
  const out = mkdtempSync(join(tmpdir(), 'muster-npm-'));
  const { order } = assemble(fakeBinaries('v9.9.9-rc.1'), 'v9.9.9-rc.1', out);
  assert.equal(order.at(-1), '@futurelastic/muster', 'launcher is published last');
  assert.equal(order.length, 5);
  for (const name of order) {
    const dir = join(out, name.replace('@futurelastic/', ''));
    const [info] = JSON.parse(execFileSync('npm', ['pack', '--dry-run', '--json', '--ignore-scripts'], { cwd: dir, encoding: 'utf8' }));
    const files = info.files.map((f) => f.path).sort();
    const want = name === '@futurelastic/muster'
      ? ['LICENSE', 'README.md', 'bin/muster.js', 'package.json']
      : ['LICENSE', 'README.md', 'bin/muster', 'package.json'];
    assert.deepEqual(files, want, name);
  }
});

test('launcher pins every platform package to its own exact version', () => {
  const out = mkdtempSync(join(tmpdir(), 'muster-npm-'));
  assemble(fakeBinaries('v1.4.0'), 'v1.4.0', out);
  const m = require(join(out, 'muster', 'package.json'));
  assert.equal(m.version, '1.4.0');
  assert.deepEqual(Object.keys(m.optionalDependencies).sort(), [
    '@futurelastic/muster-darwin-arm64', '@futurelastic/muster-darwin-x64',
    '@futurelastic/muster-linux-arm64', '@futurelastic/muster-linux-x64',
  ]);
  for (const v of Object.values(m.optionalDependencies)) assert.equal(v, '1.4.0');
  assert.equal(m.scripts, undefined, 'no install scripts');
  const p = require(join(out, 'muster-linux-x64', 'package.json'));
  assert.deepEqual([p.os, p.cpu], [['linux'], ['x64']]);
});

test('resolveBinary: ok, unsupported, missing, mismatched', () => {
  const root = mkdtempSync(join(tmpdir(), 'muster-res-'));
  const pkg = join(root, 'muster-linux-x64');
  mkdirSync(pkg);
  writeFileSync(join(pkg, 'package.json'), JSON.stringify({ version: '1.0.0' }));
  const resolve = (id) => {
    if (id === '@futurelastic/muster-linux-x64/package.json') return join(pkg, 'package.json');
    throw new Error('not found');
  };
  assert.equal(resolveBinary('linux', 'x64', resolve, '1.0.0'), join(pkg, 'bin', 'muster'));
  assert.throws(() => resolveBinary('win32', 'x64', resolve, '1.0.0'), /no prebuilt binary for win32-x64/);
  assert.throws(() => resolveBinary('linux', 'arm64', resolve, '1.0.0'), /is not installed/);
  assert.throws(() => resolveBinary('linux', 'x64', resolve, '1.0.1'), /version mismatch/);
});

test('the launcher runs the platform binary end to end', { skip: process.platform === 'win32' }, () => {
  const out = mkdtempSync(join(tmpdir(), 'muster-e2e-'));
  const key = `${process.platform}-${process.arch}`;
  const plat = PLATFORMS.find((p) => `${p.os}-${p.cpu}` === key);
  if (!plat) return;
  assemble(fakeBinaries('v1.0.0'), 'v1.0.0', join(out, 'pkgs'));
  // Lay the packages out as npm would: scope dir inside node_modules.
  const nm = join(out, 'node_modules', '@futurelastic');
  mkdirSync(nm, { recursive: true });
  execFileSync('cp', ['-R', join(out, 'pkgs', 'muster'), join(nm, 'muster')]);
  execFileSync('cp', ['-R', join(out, 'pkgs', `muster-${key}`), join(nm, `muster-${key}`)]);
  const got = execFileSync('node', [join(nm, 'muster', 'bin', 'muster.js'), 'serve'], { encoding: 'utf8' });
  assert.equal(got.trim(), 'muster');
});
