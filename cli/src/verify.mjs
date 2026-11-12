import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import net from 'node:net';
import { spawnProcess } from './security.mjs';
import { MockProvider } from './mock-provider.mjs';

/**
 * Finds an available TCP port on localhost.
 * @param {number} preferredPort
 * @returns {Promise<number>}
 */
export async function findAvailablePort(preferredPort = 8086) {
  // Never touch 8080!
  if (preferredPort === 8080) {
    preferredPort = 8086;
  }

  const isPortAvailable = (port) => {
    return new Promise((resolve) => {
      const server = net.createServer();
      server.unref();
      server.on('error', () => resolve(false));
      server.listen(port, '127.0.0.1', () => {
        server.close(() => resolve(true));
      });
    });
  };

  if (await isPortAvailable(preferredPort)) {
    return preferredPort;
  }

  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const port = srv.address().port;
      srv.close(() => resolve(port));
    });
    srv.on('error', reject);
  });
}

/**
 * Sends an HTTP request and returns status, headers, and body.
 */
function sendRequest(options, postData) {
  return new Promise((resolve, reject) => {
    const req = http.request(options, (res) => {
      let body = '';
      res.on('data', (chunk) => {
        body += chunk;
      });
      res.on('end', () => {
        resolve({
          statusCode: res.statusCode,
          headers: res.headers,
          body
        });
      });
    });

    req.on('error', reject);

    if (postData) {
      req.write(postData);
    }
    req.end();
  });
}

/**
 * Waits for a server to respond on /health.
 */
async function waitForHealth(port, timeoutMs = 45000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      const res = await sendRequest({
        host: '127.0.0.1',
        port,
        path: '/health',
        method: 'GET',
        timeout: 1000
      });
      if (res.statusCode === 200) {
        return true;
      }
    } catch {
      // Retry after short pause
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`Timed out waiting for Bifrost gateway on port ${port} after ${timeoutMs}ms`);
}

/**
 * Runs live end-to-end verification of an isolated gateway and StillMark.
 * @param {string} binaryPath
 * @param {object} options
 */
export async function runGatewayVerification(binaryPath, options = {}) {
  const { preferredPort = 8086, verbose = false, legacy = false } = options;

  if (!fs.existsSync(binaryPath)) {
    throw new Error(`Cannot verify: Gateway binary not found at '${binaryPath}'`);
  }

  // 1. Start mock provider
  const mockProvider = new MockProvider();
  await mockProvider.start();

  // 2. Select safe verification port
  const gatewayPort = await findAvailablePort(preferredPort);
  if (gatewayPort === 8080) {
    throw new Error('Port 8080 is reserved. Verification refuses to touch development gateway.');
  }

  // 3. Create isolated disposable staging environment
  const testDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-verify-'));
  const testConfigPath = path.join(testDir, 'config.json');

  const pluginEntry = legacy
    ? {
        name: 'cacheguard',
        type: 'shared_object',
        path: path.resolve(binaryPath, '..', '..', 'plugins', 'cacheguard', 'build', 'cacheguard.so'),
        enabled: true,
        config: { diagnostic_logging: true }
      }
    : {
        name: 'stillmark',
        enabled: true,
        config: {
          max_sessions: 1000,
          session_ttl: '10m',
          diagnostic_logging: true,
          expose_schema_paths: false
        }
      };

  const testConfig = {
    client: {
      port: gatewayPort,
      host: '127.0.0.1',
      enforce_auth_on_inference: true
    },
    governance: {
      virtual_keys: [
        {
          id: 'vk-verify-test',
          value: 'sk-bf-verify-key',
          name: 'verify-key',
          is_active: true,
          provider_configs: [
            {
              provider: 'openai',
              allowed_models: ['*'],
              key_ids: ['*'],
              weight: 1.0
            }
          ]
        }
      ]
    },
    providers: {
      openai: {
        keys: [
          {
            name: 'mock-key',
            value: 'sk-mock-key-for-local-verification',
            models: ['mock-gpt-4o', 'gpt-4o']
          }
        ],
        network_config: {
          base_url: mockProvider.url
        }
      }
    },
    plugins: [pluginEntry]
  };

  fs.writeFileSync(testConfigPath, JSON.stringify(testConfig, null, 2), { mode: 0o600 });

  // 4. Launch isolated Bifrost gateway process
  const gatewayArgs = [
    '-port',
    String(gatewayPort),
    '-host',
    '127.0.0.1',
    '-app-dir',
    testDir,
    '-log-level',
    verbose ? 'debug' : 'error'
  ];

  let gatewayProc = null;
  const testChecks = [];

  try {
    gatewayProc = spawnProcess(binaryPath, gatewayArgs, {
      cwd: testDir,
      stdio: verbose ? 'inherit' : 'pipe'
    });

    gatewayProc.on('error', (err) => {
      throw new Error(`Failed to spawn gateway process: ${err.message}`);
    });

    // Wait for gateway to become ready
    await waitForHealth(gatewayPort);

    // 5. Test Check 1: Tracked Request (Turn 1 - Baseline)
    const sessionID = `sess-verify-${Date.now()}`;
    const payloadTurn1 = JSON.stringify({
      model: 'openai/mock-gpt-4o',
      messages: [
        { role: 'system', content: 'You are a test assistant.' },
        { role: 'user', content: 'Verify Turn 1' }
      ]
    });

    const resTurn1 = await sendRequest(
      {
        host: '127.0.0.1',
        port: gatewayPort,
        path: '/v1/chat/completions',
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'x-bf-vk': 'sk-bf-verify-key',
          'X-Stillmark-Session-ID': sessionID,
          'X-Cacheguard-Session-ID': sessionID
        }
      },
      payloadTurn1
    );

    if (verbose) {
      console.log('Turn 1 Status:', resTurn1.statusCode);
      console.log('Turn 1 Headers:', resTurn1.headers);
      console.log('Turn 1 Body:', resTurn1.body);
    }

    const check1Pass =
      resTurn1.statusCode === 200 &&
      (resTurn1.headers['x-stillmark-status'] === 'active' || resTurn1.headers['x-cacheguard-status'] === 'active') &&
      (resTurn1.headers['x-stillmark-diagnosis'] === 'initial_request' || resTurn1.headers['x-cacheguard-diagnosis'] === 'initial_request');

    testChecks.push({
      name: 'Initial Turn Baseline Recording',
      passed: check1Pass,
      status: resTurn1.statusCode,
      stillmarkStatus: resTurn1.headers['x-stillmark-status'] || resTurn1.headers['x-cacheguard-status'],
      diagnosis: resTurn1.headers['x-stillmark-diagnosis'] || resTurn1.headers['x-cacheguard-diagnosis']
    });

    // 6. Test Check 2: Tracked Request (Turn 2 - Structurally Stable Repeat)
    const resTurn2 = await sendRequest(
      {
        host: '127.0.0.1',
        port: gatewayPort,
        path: '/v1/chat/completions',
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'x-bf-vk': 'sk-bf-verify-key',
          'X-Stillmark-Session-ID': sessionID,
          'X-Cacheguard-Session-ID': sessionID
        }
      },
      payloadTurn1
    );

    const diagTurn2 = resTurn2.headers['x-stillmark-diagnosis'] || resTurn2.headers['x-cacheguard-diagnosis'];
    const check2Pass =
      resTurn2.statusCode === 200 &&
      ['structurally_stable', 'stable_hit', 'stable_miss'].includes(diagTurn2);

    testChecks.push({
      name: 'Structurally Stable Repeat Turn',
      passed: check2Pass,
      status: resTurn2.statusCode,
      diagnosis: diagTurn2
    });

    // 7. Test Check 3: Untracked Request (No Session ID)
    const resTurn3 = await sendRequest(
      {
        host: '127.0.0.1',
        port: gatewayPort,
        path: '/v1/chat/completions',
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'x-bf-vk': 'sk-bf-verify-key'
        }
      },
      payloadTurn1
    );

    const check3Pass =
      resTurn3.statusCode === 200 &&
      (resTurn3.headers['x-stillmark-diagnosis'] === 'untracked' || !resTurn3.headers['x-stillmark-diagnosis']);

    testChecks.push({
      name: 'Untracked Request Pass-through',
      passed: check3Pass,
      status: resTurn3.statusCode,
      diagnosis: resTurn3.headers['x-stillmark-diagnosis'] || 'none'
    });

    const allPassed = testChecks.every((c) => c.passed);

    return {
      success: allPassed,
      port: gatewayPort,
      checks: testChecks
    };
  } finally {
    // 8. Teardown gateway process safely
    if (gatewayProc && gatewayProc.pid) {
      try {
        gatewayProc.kill('SIGTERM');
        // Give it 1 second to exit gracefully
        await new Promise((r) => setTimeout(r, 1000));
        if (gatewayProc.exitCode === null) {
          gatewayProc.kill('SIGKILL');
        }
      } catch {
        // ignore
      }
    }

    // Teardown mock provider
    await mockProvider.stop();

    // Clean up temporary test directory
    try {
      fs.rmSync(testDir, { recursive: true, force: true });
    } catch {
      // ignore
    }
  }
}