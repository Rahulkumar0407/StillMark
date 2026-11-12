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

test('install: dry-run mode leaves filesystem untouched', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-dry-'));
  try {
    // Scaffold minimal compatible source
    fs.writeFileSync(path.join(tmpDir, 'go.work'), 'go 1.27.3\nuse ( ./core ./transports ./plugins/stillmark )\n');
    fs.mkdirSync(path.join(tmpDir, 'core'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'core', 'go.mod'), 'module github.com/maximhq/bifrost/core\n');
    fs.mkdirSync(path.join(tmpDir, 'transports', 'bifrost-http', 'server'), { recursive: true });
    fs.writeFileSync(
      path.join(tmpDir, 'transports', 'bifrost-http', 'server', 'plugins.go'),
      'package server\nimport "github.com/maximhq/bifrost/plugins/stillmark"\nfunc load() { _ = stillmark.PluginName }\n'
    );
    fs.mkdirSync(path.join(tmpDir, 'plugins', 'stillmark'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'plugins', 'stillmark', 'stillmark.go'), 'package stillmark\n');

    const configPath = path.join(tmpDir, 'config.json');
    const initialConfig = { client: { port: 8086 }, plugins: [] };
    fs.writeFileSync(configPath, JSON.stringify(initialConfig), { mode: 0o600 });

    const result = await runInstall({
      bifrostPath: tmpDir,
      configPath,
      dryRun: true,
      skipVerify: true
    });

    assert.equal(result.built.dryRun, true);
    assert.equal(result.configured.dryRun, true);

    // Verify config on disk was NOT modified during dry-run
    const diskContent = fs.readFileSync(configPath, 'utf8');
    assert.equal(JSON.parse(diskContent).plugins.length, 0);

    // Verify no backup file created
    const files = fs.readdirSync(tmpDir);
    const backupFiles = files.filter((f) => f.includes('.bak.'));
    assert.equal(backupFiles.length, 0);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('install: blocks clean Bifrost source (Scenario B) with diagnostic error', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-block-'));
  try {
    fs.writeFileSync(path.join(tmpDir, 'go.work'), 'go 1.27.3\nuse ( ./core ./transports )\n');
    fs.mkdirSync(path.join(tmpDir, 'core'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'core', 'go.mod'), 'module github.com/maximhq/bifrost/core\n');
    fs.mkdirSync(path.join(tmpDir, 'transports', 'bifrost-http', 'server'), { recursive: true });
    fs.writeFileSync(
      path.join(tmpDir, 'transports', 'bifrost-http', 'server', 'plugins.go'),
      'package server\nfunc load() {}\n'
    );

    await assert.rejects(async () => {
      await runInstall({
        bifrostPath: tmpDir,
        skipVerify: true
      });
    }, /Installation Blocked \[Scenario B/);
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

