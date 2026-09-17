import {readFile, mkdir, writeFile, access} from 'node:fs/promises';
import {join, dirname} from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';
import {createRequire} from 'node:module';
import {randomUUID} from 'node:crypto';
import {homedir} from 'node:os';

const installed = join(process.env.APPDATA, 'npm/node_modules/@deepseek-ai/dsh');
const requireHarness = createRequire(join(installed, 'package.json'));
const yaml = requireHarness('js-yaml');
const {withFileLock, writeFileAtomic} = await import(pathToFileURL(
  join(installed, 'node_modules/@deepseek-ai/dsh-atomic-write/lib/index.js')).href);

export async function install(profile, modulePath, backupRoot) {
  await access(modulePath);
  return withFileLock(profile, async () => {
    const original = await readFile(profile, 'utf8');
    // Same scalar semantics as Harness; executable !!js profiles are preserved
    // and rejected instead of evaluating expressions in the installer.
    const rows = yaml.load(original, {schema: yaml.JSON_SCHEMA});
    if (!Array.isArray(rows)) throw new Error('Expected a declarative Harness patch list');
    const row = {id: 'uart2llm-llm-scheduler', name: pathToFileURL(modulePath).href,
      config: {provider: 'uart2llm', maxActive: 4, maxQueued: 16, waitMs: 120000}};
    const matches = rows.flatMap(entry => entry?.insert ?? []).filter(e => e?.id === row.id);
    if (matches.length) {
      if (matches.length !== 1 || JSON.stringify(matches[0]) !== JSON.stringify(row)) {
        throw new Error('Existing scheduler entry differs; preserved');
      }
      return 'already-configured';
    }
    await mkdir(backupRoot, {recursive: true, mode: 0o700});
    await writeFile(join(backupRoot, 'cordis.patch-'+randomUUID()+'.yml'), original, {flag: 'wx', mode: 0o600});
    rows.push({insert: [row]});
    if (await readFile(profile, 'utf8') !== original) throw new Error('Profile changed outside writer lock; preserved');
    await writeFileAtomic(profile, JSON.stringify(rows, null, 2)+'\n', {mode: 0o600});
    return 'installed';
  });
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const status = await install(join(homedir(), '.dsh/profiles/web/cordis.patch.yml'),
    join(dirname(fileURLToPath(import.meta.url)), 'scheduler.mjs'),
    join(process.env.LOCALAPPDATA, 'uart2llm/harness-validation'));
  console.log('Harness scheduler: '+status);
}
