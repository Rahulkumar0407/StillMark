import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { runCommand } from './security.mjs';

const CLI_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

/** Files copied from the plugin source into <bifrost>/plugins/stillmark. */
function isPluginFile(name) {
  return name.endsWith('.go') || ['go.mod', 'go.sum', 'Makefile', 'README.md'].includes(name);
}

function isPluginSourceDir(dir) {
  return fs.existsSync(path.join(dir, 'main.go')) && fs.existsSync(path.join(dir, 'go.mod'));
}

/**
 * Finds the StillMark plugin Go source: --plugin-src, the repo checkout
 * (../plugins/stillmark), or the copy bundled in the npm package (cli/plugin).
 * @param {string} explicit
 */
export function resolvePluginSource(explicit = '') {
  const candidates = explicit
    ? [path.resolve(explicit)]
    : [path.resolve(CLI_ROOT, '..', 'plugins', 'stillmark'), path.join(CLI_ROOT, 'plugin')];
  for (const dir of candidates) {
    if (isPluginSourceDir(dir)) return dir;
  }
  throw new Error(
    `StillMark plugin source not found (looked in: ${candidates.join(', ')}). ` +
      'Pass --plugin-src <path/to/StillMark/plugins/stillmark>.'
  );
}

export function pluginPaths(bifrostDir) {
  const pluginDir = path.join(bifrostDir, 'plugins', 'stillmark');
  return {
    pluginDir,
    soPath: path.join(pluginDir, 'build', 'stillmark.so'),
    gatewayPath: path.join(bifrostDir, 'dist', 'bifrost-http')
  };
}

/**
 * Copies the plugin source into <bifrost>/plugins/stillmark. Stale top-level
 * plugin files (from an older copy) are removed; build/ and subdirectories are kept.
 */
export function syncPluginSource(srcDir, bifrostDir, { dryRun = false } = {}) {
  const { pluginDir } = pluginPaths(bifrostDir);
  const files = fs.readdirSync(srcDir).filter((f) => isPluginFile(f) && fs.statSync(path.join(srcDir, f)).isFile());
  if (path.resolve(srcDir) === path.resolve(pluginDir)) {
    return { pluginDir, copied: [], removed: [], inPlace: true };
  }
  const stale = fs.existsSync(pluginDir)
    ? fs.readdirSync(pluginDir).filter((f) => isPluginFile(f) && !files.includes(f) && fs.lstatSync(path.join(pluginDir, f)).isFile())
    : [];
  if (!dryRun) {
    fs.mkdirSync(pluginDir, { recursive: true });
    for (const f of stale) fs.unlinkSync(path.join(pluginDir, f));
    for (const f of files) fs.copyFileSync(path.join(srcDir, f), path.join(pluginDir, f));
  }
  return { pluginDir, copied: files, removed: stale, inPlace: false };
}

/**
 * Makes sure Bifrost's Go workspace (go.work, git-ignored) includes the plugin.
 * Mirrors Bifrost's `make setup-workspace` when go.work does not exist yet.
 */
export async function ensureWorkspace(bifrostDir, { dryRun = false } = {}) {
  const goWork = path.join(bifrostDir, 'go.work');
  if (fs.existsSync(goWork)) {
    const content = fs.readFileSync(goWork, 'utf8');
    if (/\.\/plugins\/stillmark\b/.test(content)) return { action: 'present' };
    if (!dryRun) await runCommand('go', ['work', 'use', './plugins/stillmark'], { cwd: bifrostDir });
    return { action: 'added' };
  }
  if (!dryRun) {
    const roots = ['cli', 'core', 'framework', 'transports'].filter((d) => fs.existsSync(path.join(bifrostDir, d, 'go.mod')));
    await runCommand('go', ['work', 'init', ...roots.map((d) => `./${d}`)], { cwd: bifrostDir });
    const pluginsRoot = path.join(bifrostDir, 'plugins');
    const plugins = fs
      .readdirSync(pluginsRoot)
      .filter((d) => fs.existsSync(path.join(pluginsRoot, d, 'go.mod')))
      .map((d) => `./plugins/${d}`);
    if (!plugins.includes('./plugins/stillmark')) plugins.push('./plugins/stillmark');
    await runCommand('go', ['work', 'use', ...plugins], { cwd: bifrostDir });
  }
  return { action: 'created' };
}

async function buildAtomically(finalPath, args, cwd) {
  fs.mkdirSync(path.dirname(finalPath), { recursive: true, mode: 0o755 });
  const tmp = path.join(path.dirname(finalPath), `.${path.basename(finalPath)}.${process.pid}.${Date.now()}.tmp`);
  try {
    const res = await runCommand('go', ['build', ...args, '-o', tmp, '.'], {
      cwd,
      env: { ...process.env, CGO_ENABLED: '1' } // .so plugins and Bifrost's sqlite store need cgo
    });
    if (!fs.existsSync(tmp)) throw new Error(`go build succeeded but produced no file at '${tmp}'`);
    fs.chmodSync(tmp, 0o755);
    fs.renameSync(tmp, finalPath);
    return (res.stdout + '\n' + res.stderr).trim();
  } finally {
    if (fs.existsSync(tmp)) fs.rmSync(tmp, { force: true });
  }
}

/**
 * Builds the Bifrost gateway from the same checkout (a Go plugin only loads into
 * a host built from identical dependency versions), then the stillmark.so plugin.
 */
export async function buildGatewayAndPlugin(bifrostDir, { dryRun = false } = {}) {
  const { pluginDir, soPath, gatewayPath } = pluginPaths(bifrostDir);
  if (dryRun) return { dryRun: true, gatewayPath, soPath };

  // Bifrost embeds its web UI; a source checkout without a built UI needs a placeholder to compile.
  const uiDir = path.join(bifrostDir, 'transports', 'bifrost-http', 'ui');
  fs.mkdirSync(uiDir, { recursive: true });
  if (fs.readdirSync(uiDir).length === 0) {
    fs.writeFileSync(path.join(uiDir, 'index.html'), '<html></html>\n');
  }

  try {
    await buildAtomically(gatewayPath, [], path.join(bifrostDir, 'transports', 'bifrost-http'));
  } catch (err) {
    throw new Error(`Failed to build the Bifrost gateway:\n${err.message}`);
  }
  try {
    await buildAtomically(soPath, ['-buildmode=plugin'], pluginDir);
  } catch (err) {
    throw new Error(`Failed to build stillmark.so:\n${err.message}`);
  }
  return { dryRun: false, gatewayPath, soPath };
}
