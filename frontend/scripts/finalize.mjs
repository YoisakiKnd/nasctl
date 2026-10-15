import { readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';

const output = new URL('../../internal/hubweb/web/', import.meta.url);
const htmlFile = new URL('index.html', output);
let html = await readFile(htmlFile, 'utf8');
for (const name of ['app.js', 'app.css']) {
  const data = await readFile(new URL(name, output));
  const hash = createHash('sha256').update(data).digest('hex').slice(0, 12);
  html = html.replaceAll(`"/${name}"`, `"/${name}?v=${hash}"`);
}
await writeFile(htmlFile, html);
