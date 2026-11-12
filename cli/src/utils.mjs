import process from 'node:process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

export const COLORS = {
  reset: '\x1b[0m',
  bright: '\x1b[1m',
  red: '\x1b[31m',
  green: '\x1b[32m',
  yellow: '\x1b[33m',
  blue: '\x1b[34m',
  magenta: '\x1b[35m',
  cyan: '\x1b[36m',
  gray: '\x1b[90m'
};

const supportsColor = Boolean(process.stdout.isTTY && !process.env.NO_COLOR);

export function colorize(colorName, text) {
  if (!supportsColor || !COLORS[colorName]) {
    return String(text);
  }
  return `${COLORS[colorName]}${text}${COLORS.reset}`;
}

export function getCliVersion() {
  try {
    const pkgPath = path.resolve(__dirname, '..', 'package.json');
    const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));
    return pkg.version || '0.1.0';
  } catch {
    return '0.1.0';
  }
}

export class Logger {
  constructor(options = {}) {
    this.verbose = Boolean(options.verbose);
    this.jsonMode = Boolean(options.json);
  }

  info(msg) {
    if (!this.jsonMode) {
      console.log(msg);
    }
  }

  success(msg) {
    if (!this.jsonMode) {
      console.log(colorize('green', `✓ ${msg}`));
    }
  }

  warn(msg) {
    if (!this.jsonMode) {
      console.warn(colorize('yellow', `⚠ ${msg}`));
    }
  }

  error(msg) {
    if (!this.jsonMode) {
      console.error(colorize('red', `✗ ${msg}`));
    }
  }

  debug(msg) {
    if (this.verbose && !this.jsonMode) {
      console.log(colorize('gray', `[DEBUG] ${msg}`));
    }
  }

  header(title) {
    if (!this.jsonMode) {
      console.log('='.repeat(80));
      console.log(`  ${title.toUpperCase()}`);
      console.log('='.repeat(80));
    }
  }

  section(title) {
    if (!this.jsonMode) {
      console.log(`\n--- ${title} ---`);
    }
  }

  outputJson(data) {
    if (this.jsonMode) {
      console.log(JSON.stringify(data, null, 2));
    }
  }
}

