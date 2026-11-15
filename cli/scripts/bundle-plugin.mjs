// Copies the StillMark Go plugin source into cli/plugin so the npm package can
// install StillMark into a stock Bifrost checkout without cloning this repo.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const cliRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const src = path.resolve(cliRoot, '..', 'plugins', 'stillmark');
const dest = path.join(cliRoot, 'plugin');

if (!fs.existsSync(path.join(src, 'main.go'))) {
  console.error(`bundle-plugin: plugin source not found at ${src}`);
  process.exit(1);
}

fs.rmSync(dest, { recursive: true, force: true });
fs.mkdirSync(dest);
const keep = (f) => (f.endsWith('.go') || ['go.mod', 'go.sum', 'Makefile', 'README.md'].includes(f));
const files = fs.readdirSync(src).filter((f) => keep(f) && fs.statSync(path.join(src, f)).isFile());
for (const f of files) fs.copyFileSync(path.join(src, f), path.join(dest, f));
console.log(`bundle-plugin: copied ${files.length} files to ${dest}`);
