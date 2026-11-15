import path from 'node:path';
import { runDoctor } from './doctor.mjs';
import { resolvePluginSource, syncPluginSource, ensureWorkspace, buildGatewayAndPlugin } from './builder.mjs';
import { activateStillmarkConfig } from './config.mjs';
import { runGatewayVerification } from './verify.mjs';

/**
 * Installs StillMark into a Bifrost source checkout:
 * copy plugin source -> register in go.work -> build gateway + stillmark.so
 * -> (optional) activate in config.json -> verify on an isolated gateway.
 * @param {object} options
 * @param {(msg: string) => void} [onStep] progress callback
 */
export async function runInstall(options = {}, onStep = () => {}) {
  const {
    bifrostPath,
    configPath,
    pluginSrc = '',
    dryRun = false,
    verbose = false,
    skipVerify = false,
    port,
    customConfig = {}
  } = options;

  if (!bifrostPath) {
    throw new Error('Missing required option: --bifrost <path> (path to a Bifrost source checkout)');
  }
  const absBifrost = path.resolve(bifrostPath);

  // 1. Pre-flight checks
  const doc = await runDoctor({ bifrostPath: absBifrost, configPath });
  if (!doc.environment.go.ok) {
    throw new Error(`Incompatible Go toolchain: ${doc.environment.go.version}. StillMark requires Go 1.22+.`);
  }
  if (!doc.bifrost || !doc.bifrost.supported) {
    const b = doc.bifrost || {};
    throw new Error(
      `Installation Blocked [Scenario ${b.scenario || 'UNKNOWN'}: ${b.scenarioName || 'Invalid Path'}]\n\n` +
        (b.reasons || []).join('\n')
    );
  }

  const results = { bifrostPath: absBifrost, dryRun, source: null, workspace: null, built: null, configured: null, verified: null };

  // 2. Copy plugin source into the checkout
  const srcDir = resolvePluginSource(pluginSrc);
  onStep(`Copying StillMark plugin source from ${srcDir}`);
  const sync = syncPluginSource(srcDir, absBifrost, { dryRun });
  results.source = { from: srcDir, to: sync.pluginDir, files: sync.copied.length, removedStale: sync.removed };

  // 3. Go workspace
  onStep('Registering plugin in go.work');
  results.workspace = await ensureWorkspace(absBifrost, { dryRun });

  // 4. Build gateway and plugin from the same source tree
  onStep('Building Bifrost gateway and stillmark.so (first build can take a few minutes)');
  const built = await buildGatewayAndPlugin(absBifrost, { dryRun });
  results.built = built;

  // 5. Activate in config.json
  if (configPath) {
    onStep(`Activating StillMark in ${configPath}`);
    const cfg = activateStillmarkConfig(configPath, { dryRun, pluginPath: built.soPath, customConfig });
    results.configured = { configPath: cfg.configPath, backupPath: cfg.backupPath, entry: cfg.activatedEntry, dryRun };
  }

  // 6. Verify on an isolated gateway with a local mock provider
  if (!skipVerify && !dryRun) {
    onStep('Verifying on an isolated gateway with a local mock provider');
    try {
      const v = await runGatewayVerification(built, { verbose, preferredPort: port });
      results.verified = { success: v.success, port: v.port, checks: v.checks };
    } catch (err) {
      results.verified = { success: false, error: err.message, checks: [] };
    }
  }

  return results;
}
