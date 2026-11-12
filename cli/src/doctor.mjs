import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { runCommand, validatePathWithin } from './security.mjs';

/**
 * Checks system environment: Node.js version and Go toolchain.
 */
export async function checkEnvironment() {
  const nodeVersion = process.version;
  const majorNode = parseInt(nodeVersion.slice(1).split('.')[0], 10);
  const nodeOk = majorNode >= 18;

  let goVersion = null;
  let goOk = false;
  let goMajor = 0;
  let goMinor = 0;

  try {
    const { stdout } = await runCommand('go', ['version']);
    goVersion = stdout.trim();
    // Parse "go version go1.27.3 linux/amd64"
    const match = goVersion.match(/go(\d+)\.(\d+)/);
    if (match) {
      goMajor = parseInt(match[1], 10);
      goMinor = parseInt(match[2], 10);
      // Bifrost requires Go 1.22+ (workspace pinned at 1.27.3)
      goOk = goMajor > 1 || (goMajor === 1 && goMinor >= 22);
    }
  } catch {
    goVersion = 'Not found in PATH';
  }

  return {
    node: {
      version: nodeVersion,
      ok: nodeOk
    },
    go: {
      version: goVersion,
      ok: goOk,
      major: goMajor,
      minor: goMinor
    }
  };
}

/**
 * Inspects a Bifrost directory and determines its installation compatibility scenario.
 * @param {string} bifrostPath
 */
export async function inspectBifrostCheckout(bifrostPath) {
  if (!bifrostPath) {
    return {
      exists: false,
      scenario: 'NONE',
      scenarioName: 'No directory provided',
      reasons: ['No Bifrost directory was specified (--bifrost <path>)']
    };
  }

  const absPath = path.resolve(bifrostPath);
  if (!fs.existsSync(absPath)) {
    return {
      exists: false,
      scenario: 'NONE',
      scenarioName: 'Directory does not exist',
      reasons: [`Specified path '${absPath}' does not exist.`]
    };
  }

  validatePathWithin(path.dirname(absPath), absPath);

  const isDirectory = fs.statSync(absPath).isDirectory();
  if (!isDirectory) {
    // Single binary file provided (e.g. /usr/local/bin/bifrost)
    return {
      exists: true,
      absPath,
      scenario: 'C',
      scenarioName: 'Official Precompiled Binary / Docker Installation',
      reasons: [
        'A single executable file was provided instead of a Go source checkout.',
        'Native StillMark cannot be injected into a precompiled Go binary.',
        'Bifrost must be compiled from source with StillMark registered.'
      ],
      supported: false
    };
  }

  // Check if this is a Go source checkout
  const hasGoWork = fs.existsSync(path.join(absPath, 'go.work'));
  const hasTransports = fs.existsSync(path.join(absPath, 'transports', 'bifrost-http'));
  const hasCore = fs.existsSync(path.join(absPath, 'core', 'go.mod'));

  if (!hasGoWork || !hasTransports || !hasCore) {
    // Check if it's a directory containing only a precompiled binary
    const hasBin = fs.existsSync(path.join(absPath, 'bifrost-http')) || fs.existsSync(path.join(absPath, 'bifrost'));
    return {
      exists: true,
      absPath,
      scenario: 'C',
      scenarioName: 'Official Precompiled Binary or Incomplete Directory',
      reasons: [
        'Directory lacks Bifrost Go workspace files (go.work, core/go.mod, transports/bifrost-http).',
        'Native plugins require Go source code to compile into the gateway binary.'
      ],
      supported: false
    };
  }

  // Detect Git revision if present
  let gitRevision = 'unknown';
  try {
    const { stdout } = await runCommand('git', ['rev-parse', '--short', 'HEAD'], { cwd: absPath });
    gitRevision = stdout.trim();
  } catch {
    // Not a git repo or git not available
  }

  // Check for Native StillMark presence
  const stillmarkDir = path.join(absPath, 'plugins', 'stillmark');
  const hasStillmarkSource = fs.existsSync(stillmarkDir) && fs.existsSync(path.join(stillmarkDir, 'stillmark.go'));

  // Check for StillMark registration in server/plugins.go
  const pluginsGoPath = path.join(absPath, 'transports', 'bifrost-http', 'server', 'plugins.go');
  let isPluginsGoRegistered = false;
  if (fs.existsSync(pluginsGoPath)) {
    const pluginsGoContent = fs.readFileSync(pluginsGoPath, 'utf8');
    isPluginsGoRegistered = pluginsGoContent.includes('stillmark.PluginName') && pluginsGoContent.includes('plugins/stillmark');
  }

  // Check for StillMark in go.work
  const goWorkPath = path.join(absPath, 'go.work');
  let isGoWorkRegistered = false;
  if (fs.existsSync(goWorkPath)) {
    const goWorkContent = fs.readFileSync(goWorkPath, 'utf8');
    isGoWorkRegistered = goWorkContent.includes('./plugins/stillmark');
  }

  // Check for Legacy CacheGuard presence
  const cacheguardDir = path.join(absPath, 'plugins', 'cacheguard');
  const hasLegacySource = fs.existsSync(cacheguardDir) && fs.existsSync(path.join(cacheguardDir, 'main.go'));

  if (hasStillmarkSource && isPluginsGoRegistered && isGoWorkRegistered) {
    return {
      exists: true,
      absPath,
      gitRevision,
      scenario: 'A',
      scenarioName: 'Compatible Bifrost Source with Native StillMark',
      supported: true,
      details: {
        hasStillmarkSource,
        isPluginsGoRegistered,
        isGoWorkRegistered,
        hasLegacySource
      }
    };
  }

  if (!hasStillmarkSource || !isPluginsGoRegistered || !isGoWorkRegistered) {
    const missingItems = [];
    if (!hasStillmarkSource) missingItems.push('plugins/stillmark directory and source files');
    if (!isPluginsGoRegistered) missingItems.push('plugin registration in transports/bifrost-http/server/plugins.go');
    if (!isGoWorkRegistered) missingItems.push('./plugins/stillmark entry in go.work');

    // Check if legacy source is present
    if (hasLegacySource) {
      return {
        exists: true,
        absPath,
        gitRevision,
        scenario: 'D',
        scenarioName: 'Bifrost Source with Legacy CacheGuard (.so plugin)',
        supported: false,
        legacySupported: true,
        reasons: [
          'Native StillMark is not registered, but legacy CacheGuard (.so) was detected.',
          'To install legacy shared-object plugin, pass --legacy.',
          'Warning: Shared-object plugins (.so) require CGO and exact compiler ABI matching.'
        ],
        details: {
          missingItems,
          hasLegacySource
        }
      };
    }

    return {
      exists: true,
      absPath,
      gitRevision,
      scenario: 'B',
      scenarioName: 'Clean Bifrost Source Checkout without StillMark',
      supported: false,
      reasons: [
        'Native StillMark is not registered in this Bifrost checkout.',
        `Missing: ${missingItems.join(', ')}.`,
        'To install, checkout the StillMark branch (e.g. feat/cacheguard) or apply the StillMark patch.',
        'Arbitrary source code patching is rejected to protect codebase integrity.'
      ],
      details: {
        missingItems,
        hasLegacySource
      }
    };
  }

  return {
    exists: true,
    absPath,
    scenario: 'UNKNOWN',
    scenarioName: 'Unrecognized Configuration',
    supported: false,
    reasons: ['Unable to determine Bifrost integration state.']
  };
}

/**
 * Runs a complete diagnostic scan of environment, Bifrost source, and configuration.
 * @param {object} options
 */
export async function runDoctor(options = {}) {
  const { bifrostPath = '', configPath = '' } = options;
  const env = await checkEnvironment();
  const bifrost = bifrostPath ? await inspectBifrostCheckout(bifrostPath) : null;

  let configInfo = null;
  if (configPath) {
    try {
      const absConfig = path.resolve(configPath);
      if (fs.existsSync(absConfig)) {
        const raw = fs.readFileSync(absConfig, 'utf8');
        const parsed = JSON.parse(raw);
        const plugins = Array.isArray(parsed.plugins) ? parsed.plugins : [];
        const stillmarkEntry = plugins.find((p) => typeof p === 'object' && p !== null && p.name === 'stillmark');
        const cacheguardEntry = plugins.find((p) => typeof p === 'object' && p !== null && p.name === 'cacheguard');

        configInfo = {
          exists: true,
          path: absConfig,
          valid: true,
          hasStillmark: Boolean(stillmarkEntry),
          stillmarkEnabled: Boolean(stillmarkEntry && stillmarkEntry.enabled),
          hasCacheguard: Boolean(cacheguardEntry),
          cacheguardEnabled: Boolean(cacheguardEntry && cacheguardEntry.enabled),
          pluginCount: plugins.length
        };
      } else {
        configInfo = {
          exists: false,
          path: absConfig,
          valid: false,
          error: 'File does not exist'
        };
      }
    } catch (err) {
      configInfo = {
        exists: true,
        path: configPath,
        valid: false,
        error: err.message
      };
    }
  }

  return {
    environment: env,
    bifrost,
    config: configInfo,
    canInstallNative: Boolean(env.go.ok && bifrost && bifrost.supported && bifrost.scenario === 'A'),
    canInstallLegacy: Boolean(env.go.ok && bifrost && bifrost.details && bifrost.details.hasLegacySource)
  };
}

