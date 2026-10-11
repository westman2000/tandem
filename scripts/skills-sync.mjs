// Canonical repo skills travel with both clients as ordinary files, not symlinks.
import { readdir, readFile, mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const check = process.argv.includes('--check');
const source = path.join(root, '.agents/skills');
const target = path.join(root, '.claude/skills');
let failed = false;
const names = [];
for (const entry of await readdir(source, { withFileTypes: true })) {
  if (!entry.isDirectory()) throw new Error(`Expected a skill directory: ${entry.name}`);
  names.push(entry.name);
  const files = await readdir(path.join(source, entry.name));
  if (files.length !== 1 || files[0] !== 'SKILL.md') {
    throw new Error(`Update the synchronizer before adding supporting files: ${entry.name}`);
  }
  const body = await readFile(path.join(source, entry.name, 'SKILL.md'));
  const destination = path.join(target, entry.name, 'SKILL.md');
  let current;
  try { current = await readFile(destination); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (body.equals(current || Buffer.alloc(0))) continue;
  if (check) {
    console.error(`Skill copy differs or is missing: ${entry.name}; run npm run skills:sync`);
    failed = true;
  } else {
    await mkdir(path.dirname(destination), { recursive: true });
    await writeFile(destination, body);
    console.log(`Updated Claude skill: ${entry.name}`);
  }
}
let destinations = [];
try { destinations = await readdir(target); } catch (error) { if (error.code !== 'ENOENT') throw error; }
for (const name of destinations) {
  if (!names.includes(name)) {
    console.error(`Unexpected Claude skill ${name}; inspect it before removing or making it canonical.`);
    failed = true;
  }
}
if (failed) process.exitCode = 1;
else console.log(`Skills ${check ? 'checked' : 'synchronized'}: ${names.length}`);
