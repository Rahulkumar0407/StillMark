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

  // A buildable Bifrost source checkout (go.work is git-ignored, so it is not required)
  const hasTransports = fs.existsSync(path.join(absPath, 'transports', 'bifrost-http'));
  const hasCore = fs.existsSync(path.join(absPath, 'core', 'go.mod'));
  const hasPluginsDir = fs.existsSync(path.join(absPath, 'plugins'));

  if (!hasTransports || !hasCore || !hasPluginsDir) {
    return {
      exists: true,
      absPath,
      scenario: 'C',
      scenarioName: 'Precompiled Binary or Incomplete Directory',
      reasons: [
        'Directory is not a Bifrost source checkout (needs core/go.mod, transports/bifrost-http, plugins/).',
        'StillMark is a Go plugin and must be compiled against the same Bifrost source as the gateway.',
        'Get one with: git clone https://github.com/maximhq/bifrost.git'
      ],
      supported: false
    };
  }

  let gitRevision = 'unknown';
  try {
    const { stdout } = await runCommand('git', ['rev-parse', '--short', 'HEAD'], { cwd: absPath });
    gitRevision = stdout.trim();
  } catch {
    // Not a git repo or git not available
  }

  const pluginDir = path.join(absPath, 'plugins', 'stillmark');
  const goWorkPath = path.join(absPath, 'go.work');
  const hasGoWork = fs.existsSync(goWorkPath);

  return {
    exists: true,
    absPath,
    gitRevision,
    scenario: 'A',
    scenarioName: 'Bifrost Source Checkout',
    supported: true,
    details: {
      stillmarkPresent: fs.existsSync(path.join(pluginDir, 'main.go')),
      pluginBuilt: fs.existsSync(path.join(pluginDir, 'build', 'stillmark.so')),
      gatewayBuilt: fs.existsSync(path.join(absPath, 'dist', 'bifrost-http')),
      hasGoWork,
      goWorkHasStillmark: hasGoWork && /\.\/plugins\/stillmark\b/.test(fs.readFileSync(goWorkPath, 'utf8'))
    }
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
    canInstall: Boolean(env.node.ok && env.go.ok && bifrost && bifrost.supported)
  };
}

