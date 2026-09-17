import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, writeFile, readFile, rm, readdir} from 'node:fs/promises';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {install} from './install.mjs';

test('Harness YAML scalar semantics and concurrent installs preserve unrelated rows', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'uart2llm-harness-install-'));
  try {
    const file = join(dir, 'patch.yml'), module = join(dir, 'scheduler.mjs');
    await writeFile(module, '');
    await writeFile(file, '- id: other\n  config:\n    reasoning: off\n    label: yes\n    version: 0123\n');
    const results = await Promise.all([install(file, module, join(dir, 'backup')), install(file, module, join(dir, 'backup'))]);
    const rows = JSON.parse(await readFile(file, 'utf8'));
    assert.equal(rows[0].config.reasoning, 'off'); assert.equal(rows[0].config.label, 'yes');
    assert.equal(rows[0].config.version, 123); assert.equal(rows.length, 2);
    assert.deepEqual(results.sort(), ['already-configured','installed']);
    assert.equal((await readdir(join(dir, 'backup'))).length, 1);
  } finally {await rm(dir, {recursive: true, force: true});}
});
test('executable profiles are refused without modification', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'uart2llm-harness-install-'));
  try {
    const file = join(dir, 'patch.yml'), module = join(dir, 'scheduler.mjs');
    const original = '- id: other\n  config: !!js process.env.EXAMPLE\n';
    await writeFile(module, ''); await writeFile(file, original);
    await assert.rejects(install(file, module, join(dir, 'backup')));
    assert.equal(await readFile(file, 'utf8'), original);
  } finally {await rm(dir, {recursive: true, force: true});}
});
