#!/usr/bin/env node
// Assemble the five npm packages for a release (#243).
//
//   node scripts/assemble-npm.mjs BINDIR TAG OUTDIR
//
//   BINDIR  where scripts/build-release.sh put muster-<TAG>-<os>-<arch>.
//   TAG     the release tag, v<digit>...; the npm version is the tag without
//           the leading "v", and is the SAME for all five packages.
//   OUTDIR  receives one directory per package, ready for `npm publish`:
//             muster-<os>-<cpu>/   the four platform packages
//             muster/              the launcher
//           and a PUBLISH_ORDER file: platform packages first, launcher last,
//           so the launcher never points at a version that does not exist yet.
//
// Everything is stamped from the tag here; the manifests under npm/ carry
// version 0.0.0 on purpose, so there is no second place for a version to rot.
import { chmodSync, copyFileSync, cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Go GOOS/GOARCH -> npm os/cpu. The package suffix is npm's vocabulary.
export const PLATFORMS = [
  { goos: 'darwin', goarch: 'arm64', os: 'darwin', cpu: 'arm64' },
  { goos: 'darwin', goarch: 'amd64', os: 'darwin', cpu: 'x64' },
  { goos: 'linux', goarch: 'amd64', os: 'linux', cpu: 'x64' },
  { goos: 'linux', goarch: 'arm64', os: 'linux', cpu: 'arm64' },
];

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const SCOPE = '@futurelastic';

export function versionFromTag(tag) {
  const m = /^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$/.exec(tag);
  if (!m) throw new Error(`'${tag}' is not a release tag (want vMAJOR.MINOR.PATCH[-prerelease])`);
  return m[1];
}

export function distTag(version) {
  return version.includes('-') ? 'next' : 'latest';
}

export function assemble(binDir, tag, outDir) {
  const version = versionFromTag(tag);
  rmSync(outDir, { recursive: true, force: true });
  mkdirSync(outDir, { recursive: true });
  const order = [];
  const optional = {};

  for (const p of PLATFORMS) {
    const suffix = `${p.os}-${p.cpu}`;
    const src = join(binDir, `muster-${tag}-${p.goos}-${p.goarch}`);
    if (!existsSync(src)) throw new Error(`missing binary ${src}`);
    const dir = join(outDir, `muster-${suffix}`);
    mkdirSync(join(dir, 'bin'), { recursive: true });
    copyFileSync(src, join(dir, 'bin', 'muster'));
    chmodSync(join(dir, 'bin', 'muster'), 0o755);
    const fill = (s) => s.replaceAll('@PLATFORM@', suffix).replaceAll('@OS@', p.os).replaceAll('@CPU@', p.cpu);
    const manifest = JSON.parse(fill(readFileSync(join(root, 'npm/platform/package.json'), 'utf8')));
    manifest.version = version;
    writeFileSync(join(dir, 'package.json'), JSON.stringify(manifest, null, 2) + '\n');
    writeFileSync(join(dir, 'README.md'), fill(readFileSync(join(root, 'npm/platform/README.md'), 'utf8')));
    copyFileSync(join(root, 'LICENSE'), join(dir, 'LICENSE'));
    optional[`${SCOPE}/muster-${suffix}`] = version; // exact, never a range
    order.push(`${SCOPE}/muster-${suffix}`);
  }

  const dir = join(outDir, 'muster');
  cpSync(join(root, 'npm/launcher/bin'), join(dir, 'bin'), { recursive: true });
  chmodSync(join(dir, 'bin', 'muster.js'), 0o755);
  const manifest = JSON.parse(readFileSync(join(root, 'npm/launcher/package.json'), 'utf8'));
  manifest.version = version;
  manifest.optionalDependencies = optional;
  writeFileSync(join(dir, 'package.json'), JSON.stringify(manifest, null, 2) + '\n');
  copyFileSync(join(root, 'README.md'), join(dir, 'README.md'));
  copyFileSync(join(root, 'LICENSE'), join(dir, 'LICENSE'));
  order.push(`${SCOPE}/muster`);

  writeFileSync(join(outDir, 'PUBLISH_ORDER'), order.map((n) => n.replace(`${SCOPE}/`, '')).join('\n') + '\n');
  return { version, order };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  if (process.argv.length !== 5) {
    console.error('usage: node scripts/assemble-npm.mjs BINDIR TAG OUTDIR');
    process.exit(2);
  }
  try {
    const { version, order } = assemble(resolve(process.argv[2]), process.argv[3], resolve(process.argv[4]));
    console.log(`assemble-npm: ${version} (dist-tag ${distTag(version)}): ${order.join(', ')}`);
  } catch (err) {
    console.error(`assemble-npm: ${err.message}`);
    process.exit(1);
  }
}
