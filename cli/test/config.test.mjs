import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import {
  createSafeBackup,
  writeConfigAtomically,
  activateStillmarkConfig,
  deactivateStillmarkConfig
} from '../src/config.mjs';

test('config: createSafeBackup creates collision-safe backups with secure permissions', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-cfg-'));
  try {
    const configPath = path.join(tmpDir, 'config.json');
    const content = Buffer.from('{"test": 123}');
    fs.writeFileSync(configPath, content, { mode: 0o644 });

    const bak1 = createSafeBackup(configPath, content, 0o644);
    assert.ok(fs.existsSync(bak1));
    const stat1 = fs.statSync(bak1);
    assert.equal(stat1.mode & 0o777, 0o600); // capped at 0600!

    const bak2 = createSafeBackup(configPath, content, 0o644);
    assert.ok(fs.existsSync(bak2));
    assert.notEqual(bak1, bak2, 'Second backup must not overwrite first backup');
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('config: writeConfigAtomically updates file and preserves safe permissions', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-cfg-'));
  try {
    const configPath = path.join(tmpDir, 'config.json');
    fs.writeFileSync(configPath, '{"initial": true}', { mode: 0o600 });

    writeConfigAtomically(configPath, '{"updated": true}', 0o600);
    const content = fs.readFileSync(configPath, 'utf8');
    assert.equal(JSON.parse(content).updated, true);

    // Rejects invalid JSON
    assert.throws(() => {
      writeConfigAtomically(configPath, '{not valid json', 0o600);
    }, /Refusing to write invalid JSON/);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('config: activateStillmarkConfig preserves unrelated plugins and operator settings', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-cfg-'));
  try {
    const configPath = path.join(tmpDir, 'config.json');
    const initialConfig = {
      client: { port: 8086 },
      plugins: [
        { name: 'telemetry', enabled: true },
        {
          name: 'stillmark',
          enabled: false,
          config: {
            custom_operator_key: 'preserve-me',
            max_sessions: 42
          }
        },
        { name: 'governance', enabled: true }
      ]
    };
    fs.writeFileSync(configPath, JSON.stringify(initialConfig, null, 2), { mode: 0o600 });

    const res = activateStillmarkConfig(configPath);
    assert.ok(fs.existsSync(res.backupPath));

    const updated = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    assert.equal(updated.plugins.length, 3);
    assert.equal(updated.plugins[0].name, 'telemetry');
    assert.equal(updated.plugins[2].name, 'governance');

    const sm = updated.plugins[1];
    assert.equal(sm.name, 'stillmark');
    assert.equal(sm.enabled, true);
    assert.equal(sm.config.custom_operator_key, 'preserve-me', 'Operator custom settings must be preserved');
    assert.equal(sm.config.max_sessions, 42, 'Existing operator config values must be preserved');
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('config: activateStillmarkConfig rejects malformed plugins section', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-cfg-'));
  try {
    const configPath = path.join(tmpDir, 'config.json');
    fs.writeFileSync(configPath, JSON.stringify({ plugins: 'not-an-array' }), { mode: 0o600 });

    assert.throws(() => {
      activateStillmarkConfig(configPath);
    }, /'plugins' property must be an array/);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('config: deactivateStillmarkConfig deactivates StillMark and creates backup', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-cfg-'));
  try {
    const configPath = path.join(tmpDir, 'config.json');
    const initialConfig = {
      plugins: [
        { name: 'telemetry', enabled: true },
        { name: 'stillmark', enabled: true }
      ]
    };
    fs.writeFileSync(configPath, JSON.stringify(initialConfig, null, 2), { mode: 0o600 });

    const res = deactivateStillmarkConfig(configPath, { removeEntry: false });
    assert.equal(res.modified, true);
    assert.ok(fs.existsSync(res.backupPath));

    const updated = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    assert.equal(updated.plugins.find((p) => p.name === 'stillmark').enabled, false);
    assert.equal(updated.plugins.find((p) => p.name === 'telemetry').enabled, true);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

