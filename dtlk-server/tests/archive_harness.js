// Runs upload.js's packer under node for test_web.py. Reads a JSON spec on stdin:
//   {wasm, part_size, now, roots, items: [{path, dir} | {path, data (base64), mtime}]}
// and prints {name, kind, items, tar_size, estimate, parts (base64)}; or {uniq: [[name, dir]...]}
// prints the names uniq() picks.
const fs = require('fs');
const path = require('path');

const DTLK = new Function(fs.readFileSync(path.join(__dirname, '..', 'app', 'upload.js'), 'utf8') + '\nreturn DTLK;')();
const spec = JSON.parse(fs.readFileSync(0, 'utf8'));

if (spec.uniq) {
  const used = {};
  process.stdout.write(JSON.stringify(spec.uniq.map(([name, dir]) => DTLK.uniq(name, used, dir))));
  process.exit(0);
}

const now = new Date(spec.now * 1000);
const items = spec.items.map((it) =>
  it.dir
    ? {path: it.path, dir: true}
    : {path: it.path, file: new File([Buffer.from(it.data, 'base64')], 'x', {lastModified: it.mtime * 1000})},
);

function fail(err) {
  console.error(err);
  process.exit(1);
}

function run(zstd) {
  const b = DTLK.bundle(items, spec.roots, now);
  const t = DTLK.tar(b.entries, now);
  const parts = [];
  DTLK.stream(
    t,
    zstd,
    spec.part_size,
    (blob, cb) => blob.arrayBuffer().then((buf) => cb(null, new Uint8Array(buf)), fail),
    (n, blob, pos, next) =>
      blob.arrayBuffer().then((buf) => {
        parts.push(Buffer.from(buf).toString('base64'));
        next();
      }, fail),
    () => process.stdout.write(JSON.stringify({name: b.name, kind: b.kind, items: b.items, tar_size: t.size, estimate: DTLK.estimate(t.size), parts})),
    fail,
  );
}

if (spec.wasm) {
  DTLK.instantiate(fs.readFileSync(spec.wasm), (zstd) => (zstd ? run(zstd) : fail('cannot load ' + spec.wasm)));
} else {
  run(null);
}
