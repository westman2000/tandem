// Lightweight repository conventions; no npm packages, account access or network.
import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, lstatSync, readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const errors = [];
const list = (...args) => execFileSync('git', ['ls-files', '-z', ...args], { cwd: root, encoding: 'utf8' }).split('\0').filter(Boolean);
const tracked = list('--cached');
const files = [...new Set([...tracked, ...list('--others', '--exclude-standard')])];
const required = ['AGENTS.md', 'CLAUDE.md', 'docs/STATUS.md', 'docs/MAP.md', 'docs/HANDOFF.md', 'docs/ROADMAP.md', 'docs/architecture.md', 'docs/validation.md'];
for (const file of required) {
  if (!existsSync(path.join(root, file))) errors.push(`Missing entry document: ${file}`);
}
const claude = path.join(root, 'CLAUDE.md');
if (existsSync(claude) && (lstatSync(claude).isSymbolicLink() || readFileSync(claude, 'utf8').trim() !== '@AGENTS.md')) {
  errors.push('CLAUDE.md must be an ordinary file importing @AGENTS.md; keep shared rules in AGENTS.md.');
}
for (const file of tracked) {
  if (/(^|\/)(\.tandem(?:-preview)?|\.tools|node_modules|release)(\/|$)/.test(file)
      || /(^|\/)\.env(?:\..+)?$/.test(file) && !file.endsWith('.example')
      || /\.(?:sqlite|sqlite3|db)(?:-wal|-shm)?$/.test(file)
      || /(^|\/)(?:auth\.json|credentials\.json|id_rsa|id_ed25519)$/.test(file)
      || /^(?:tandem(?:\.exe)?|web\/dist\/(?!placeholder\.txt$).+)$/.test(file)) {
    errors.push(`Local data, credentials or build output is tracked: ${file}`);
  }
}
let links = 0;
for (const file of files.filter(f => f.endsWith('.md'))) {
  const absolute = path.join(root, file);
  if (!existsSync(absolute) || !lstatSync(absolute).isFile()) continue;
  const raw = readFileSync(absolute, 'utf8');
  // Scan code fences too. Report only the location, never the private value.
  const homePath = /(?:\/(?:Users|home)\/[^\s/`"'<>]+|[a-z]:[\\/]+Users[\\/]+[^\s\\/`"'<>]+)/i;
  for (const [index, line] of raw.split(/\r?\n/).entries()) {
    if (homePath.test(line)) errors.push(`Nonportable home path in ${file}:${index + 1}; use a relative path or ~.`);
  }
  // Check ordinary inline Markdown links; URL fragments and reference links are not validated.
  const text = raw.replace(/^(`{3,}|~{3,})[^\n]*\n[\s\S]*?^\1[^\n]*$/gm, '');
  for (const match of text.matchAll(/\[[^\]\n]*\]\((<[^>\n]+>|[^\s)]+)(?:\s+"[^"\n]*")?\)/g)) {
    const target = match[1].replace(/^<|>$/g, '');
    if (/^(?:[a-z][a-z0-9+.-]*:|#|\/\/)/i.test(target)) continue;
    let clean;
    try { clean = decodeURIComponent(target.split(/[?#]/)[0]); } catch { errors.push(`Invalid link in ${file}: ${target}`); continue; }
    if (!clean) continue;
    links++;
    const resolved = path.resolve(path.dirname(absolute), clean);
    const relative = path.relative(root, resolved);
    if (relative === '..' || relative.startsWith('..' + path.sep) || path.isAbsolute(relative) || !existsSync(resolved)) {
      errors.push(`Broken or nonportable file link in ${file}: ${target}`);
    }
  }
}
const skillRoot = path.join(root, '.agents/skills');
if (!existsSync(skillRoot)) errors.push('Missing canonical .agents/skills directory');
else {
  for (const name of readdirSync(skillRoot)) {
    const file = path.join(skillRoot, name, 'SKILL.md');
    if (!existsSync(file)) { errors.push(`Missing SKILL.md: ${name}`); continue; }
    const text = readFileSync(file, 'utf8');
    const header = text.match(/^---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/)?.[1] || '';
    if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(name) || name.length > 64
        || header.match(/^name:\s*(.+)$/m)?.[1].trim() !== name || !/^description:\s*\S.+$/m.test(header)) {
      errors.push(`Invalid skill name/description: ${name}`);
    }
  }
  const result = spawnSync(process.execPath, [path.join(root, 'scripts/skills-sync.mjs'), '--check'], { cwd: root, stdio: 'inherit' });
  if (result.error || result.status !== 0) errors.push('Skill synchronization check failed');
}
if (errors.length) {
  errors.forEach(error => console.error(error));
  process.exitCode = 1;
} else console.log(`Repository checked: ${links} local links, entry documents, skill copies and tracked paths.`);
