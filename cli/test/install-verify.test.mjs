import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { runInstall } from '../src/install.mjs';
import { runUninstall } from '../src/uninstall.mjs';
import { MockProvider } from '../src/mock-provider.mjs';

test('mock-provider: starts and responds on ephemeral port', async () => {
  const provider = new MockProvider();
  await provider.start();
  try {
    assert.ok(provider.port > 0);
    assert.notEqual(provider.port, 8080);
    assert.ok(provider.url.includes('127.0.0.1'));
  } finally {
    await provider.stop();
  }
});

function scaffoldBifrost(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  fs.mkdirSync(path.join(dir, 'core'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'core', 'go.mod'), 'module github.com/maximhq/bifrost/core\n');
  fs.mkdirSync(path.join(dir, 'transports', 'bifrost-http'), { recursive: true });
  fs.mkdirSync(path.join(dir, 'plugins'), { recursive: true });
  return dir;
}

test('install: dry-run mode leaves filesystem untouched', async () => {
  const tmpDir = scaffoldBifrost('stillmark-dry-');
  try {
    const configPath = path.join(tmpDir, 'config.json');
    fs.writeFileSync(configPath, JSON.stringify({ plugins: [] }), { mode: 0o600 });

    const result = await runInstall({ bifrostPath: tmpDir, configPath, dryRun: true, skipVerify: true });

    assert.equal(result.built.dryRun, true);
    assert.equal(result.configured.dryRun, true);
    assert.ok(result.source.files > 0, 'plugin source files should be found');
    assert.equal(result.configured.entry.type, 'shared_object');

    assert.equal(JSON.parse(fs.readFileSync(configPath, 'utf8')).plugins.length, 0);
    assert.equal(fs.existsSync(path.join(tmpDir, 'plugins', 'stillmark')), false, 'no plugin copy in dry-run');
    assert.equal(fs.existsSync(path.join(tmpDir, 'go.work')), false, 'no go.work in dry-run');
    assert.equal(fs.readdirSync(tmpDir).filter((f) => f.includes('.bak.')).length, 0);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('install: blocks a directory that is not a Bifrost source checkout', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-block-'));
  try {
    fs.writeFileSync(path.join(tmpDir, 'bifrost-http'), 'binary');
    await assert.rejects(runInstall({ bifrostPath: tmpDir, skipVerify: true }), /Installation Blocked \[Scenario C/);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('uninstall: transactional deactivation updates config and creates backup', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-uninst-'));
  try {
    const configPath = path.join(tmpDir, 'config.json');
    const initialConfig = {
      plugins: [
        { name: 'stillmark', enabled: true },
        { name: 'telemetry', enabled: true }
      ]
    };
    fs.writeFileSync(configPath, JSON.stringify(initialConfig, null, 2), { mode: 0o600 });

    const uninstRes = await runUninstall({
      configPath,
      removeEntry: true
    });

    assert.equal(uninstRes.deconfigured.modified, true);
    assert.ok(fs.existsSync(uninstRes.deconfigured.backupPath));

    const updated = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    assert.equal(updated.plugins.length, 1);
    assert.equal(updated.plugins[0].name, 'telemetry');
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

