// The upload page's script (inlined at / by webupload.py). It packs what is dropped
// or chosen into the same tar.zst the CLI makes and sends it through /api/uploads.
// Plain ES5 so old browsers still parse it; those without XHR2/Blob/FileReader keep
// the page's form, which posts to /upload instead.
var DTLK = (function () {
  'use strict';
  var FRAME = 4 * 1024 * 1024; // tar bytes per zstd frame
  var RAW_BLOCK = 128 * 1024; // largest raw block for the 128 KiB window below
  var LEVEL = 3;
  var BIG = 8589934592; // 8 GiB: sizes from here on need a PAX record

  // UTF-8 bytes as a "binary string" (one char per byte)
  function utf8(s) {
    return unescape(encodeURIComponent(s));
  }

  function bytes(bin) {
    var a = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) a[i] = bin.charCodeAt(i);
    return a;
  }

  function octal(n, width) {
    var s = n.toString(8);
    while (s.length < width - 1) s = '0' + s;
    return s;
  }

  function header(name, type, size, mode, mtime) {
    var h = new Uint8Array(512);
    function put(off, s) {
      for (var i = 0; i < s.length; i++) h[off + i] = s.charCodeAt(i);
    }
    put(0, name.slice(0, 100));
    put(100, octal(mode, 8));
    put(108, octal(0, 8));
    put(116, octal(0, 8));
    put(124, octal(size, 12));
    put(136, octal(mtime, 12));
    put(148, '        ');
    put(156, type);
    put(257, 'ustar\u000000');
    var sum = 0;
    for (var i = 0; i < 512; i++) sum += h[i];
    put(148, octal(sum, 7) + '\u0000 ');
    return h;
  }

  // "<len> key=value\n" where len counts the whole record, its own digits included
  function paxRecord(key, value) {
    var body = ' ' + key + '=' + value + '\n';
    var len = body.length + String(body.length).length;
    if (String(len).length + body.length !== len) len++;
    return len + body;
  }

  function padding(n) {
    return (512 - (n % 512)) % 512;
  }

  // entries: [{path, dir, file}] in archive order, paths without a trailing slash.
  // Returns the tar stream as pieces (header bytes or File) without reading any file.
  function tar(entries, now) {
    var t = {parts: [], offs: [], size: 0};
    function add(p, len) {
      if (!len) return;
      t.parts.push(p);
      t.offs.push(t.size);
      t.size += len;
    }
    var mnow = Math.floor(now.getTime() / 1000);
    for (var i = 0; i < entries.length; i++) {
      var e = entries[i], f = e.file;
      var name = utf8(e.path + (e.dir ? '/' : ''));
      var size = e.dir ? 0 : f.size;
      var mtime = !e.dir && f.lastModified ? Math.floor(f.lastModified / 1000) : mnow;
      var pax = '';
      if (name.length > 100 || /[\x80-\xff]/.test(name)) pax += paxRecord('path', name);
      if (size >= BIG) pax += paxRecord('size', String(size));
      if (pax) {
        add(header('PaxHeader', 'x', pax.length, 420, mnow), 512);
        add(bytes(pax), pax.length);
        add(new Uint8Array(padding(pax.length)), padding(pax.length));
      }
      add(header(name, e.dir ? '5' : '0', size >= BIG ? 0 : size, e.dir ? 493 : 420, mtime), 512);
      if (size) {
        add(f, size);
        add(new Uint8Array(padding(size)), padding(size));
      }
    }
    add(new Uint8Array(1024), 1024);
    return t;
  }

  // Bytes [a, b) of the tar stream as a Blob
  function tarSlice(t, a, b) {
    var lo = 0, hi = t.parts.length - 1, out = [];
    while (lo < hi) {
      var mid = (lo + hi + 1) >> 1;
      if (t.offs[mid] <= a) lo = mid;
      else hi = mid - 1;
    }
    for (var i = lo; i < t.parts.length && t.offs[i] < b; i++) {
      var p = t.parts[i], end = i + 1 < t.parts.length ? t.offs[i + 1] : t.size;
      var s = Math.max(a, t.offs[i]) - t.offs[i], e = Math.min(b, end) - t.offs[i];
      out.push(p.subarray ? new Uint8Array(p.subarray(s, e)) : p.slice(s, e));
    }
    return new Blob(out);
  }

  // A zstd frame that stores d as-is (raw blocks), for browsers without WebAssembly
  // and for data that does not compress.
  function rawFrame(d) {
    var n = Math.max(1, Math.ceil(d.length / RAW_BLOCK));
    var f = new Uint8Array(6 + 3 * n + d.length), o = 6;
    f.set([0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38]); // magic, no flags, 128 KiB window
    for (var i = 0; i < n; i++) {
      var c = d.subarray(i * RAW_BLOCK, (i + 1) * RAW_BLOCK);
      var v = c.length * 8 + (i === n - 1 ? 1 : 0); // size << 3 | raw type | last
      f[o] = v & 255;
      f[o + 1] = (v >> 8) & 255;
      f[o + 2] = (v >> 16) & 255;
      f.set(c, o + 3);
      o += 3 + c.length;
    }
    return f;
  }

  function rawFrameSize(n) {
    return 6 + 3 * Math.max(1, Math.ceil(n / RAW_BLOCK)) + n;
  }

  // Upper bound of the upload: every frame raw
  function estimate(size) {
    var full = Math.floor(size / FRAME), rest = size % FRAME;
    return full * rawFrameSize(FRAME) + (rest ? rawFrameSize(rest) : 0);
  }

  // zstd.wasm (dtlk-server/zstd-wasm) wrapped as compress(Uint8Array) -> frame or null
  function Zstd(instance) {
    var x = instance.exports;
    if (x._initialize) x._initialize();
    var cctx = x.ZSTD_createCCtx(), cap = x.ZSTD_compressBound(FRAME);
    var src = x.malloc(FRAME), dst = x.malloc(cap);
    if (!cctx || !src || !dst) throw new Error('zstd: out of memory');
    this.compress = function (d) {
      new Uint8Array(x.memory.buffer).set(d, src);
      var n = x.ZSTD_compressCCtx(cctx, dst, cap, src, d.length, LEVEL);
      return x.ZSTD_isError(n) ? null : new Uint8Array(x.memory.buffer.slice(dst, dst + n));
    };
  }

  function instantiate(buf, done) {
    WebAssembly.compile(buf).then(function (mod) {
      var imports = {};
      WebAssembly.Module.imports(mod).forEach(function (im) {
        imports[im.module] = imports[im.module] || {};
        imports[im.module][im.name] = function () {
          return 52; // ENOSYS: zstd never does I/O
        };
      });
      return WebAssembly.instantiate(mod, imports);
    }).then(function (inst) {
      done(new Zstd(inst));
    }).then(null, function () {
      done(null);
    });
  }

  function frame(d, zstd) {
    var c = zstd && zstd.compress(d);
    return c && c.length < rawFrameSize(d.length) ? c : rawFrame(d);
  }

  // Reads the tar FRAME bytes at a time, turns each read into a zstd frame and hands
  // out the output in parts of exactly partSize bytes (the last one shorter):
  // onPart(n, blob, tarBytesRead, next) must call next() when it wants the next part.
  function stream(t, zstd, partSize, read, onPart, onEnd, onError) {
    var pos = 0, n = 0, pending = [], pendingLen = 0;
    function step() {
      if (pendingLen >= partSize || (pos >= t.size && pendingLen)) {
        var all = new Blob(pending), len = Math.min(partSize, pendingLen);
        pending = len < pendingLen ? [all.slice(len)] : [];
        pendingLen -= len;
        return onPart(++n, all.slice(0, len), pos, step);
      }
      if (pos >= t.size) return onEnd();
      var end = Math.min(pos + FRAME, t.size);
      read(tarSlice(t, pos, end), function (err, data) {
        if (err) return onError(err);
        var f = frame(data, zstd);
        pending.push(f);
        pendingLen += f.length;
        pos = end;
        step();
      });
    }
    step();
  }

  function stamp(d) {
    function p(n) {
      return (n < 10 ? '0' : '') + n;
    }
    return 'dtlk-' + p(d.getFullYear() % 100) + p(d.getMonth() + 1) + p(d.getDate()) +
      '-' + p(d.getHours()) + p(d.getMinutes()) + p(d.getSeconds());
  }

  // One top-level item stays as it is; several go into a dtlk-yymmdd-hhmmss folder.
  // items: [{path, dir, file}]; roots: their distinct top-level names in order.
  function bundle(items, roots, now) {
    var b = {name: roots[0], kind: items[0].dir ? 'dir' : 'file', entries: items, size: 0};
    if (roots.length > 1) {
      b.name = stamp(now);
      b.kind = 'dir';
      b.entries = [{path: b.name, dir: true}];
      for (var i = 0; i < items.length; i++) {
        b.entries.push({path: b.name + '/' + items[i].path, dir: items[i].dir, file: items[i].file});
      }
    }
    for (var j = 0; j < items.length; j++) b.size += items[j].dir ? 0 : items[j].file.size;
    return b;
  }

  // Top-level names must differ: the CLI refuses to unpack two "a.txt"s.
  function uniq(name, used, dir) {
    var dot = dir ? -1 : name.lastIndexOf('.'), n = name, i = 1;
    if (dot <= 0) dot = name.length;
    while (used['$' + n]) n = name.slice(0, dot) + ' (' + ++i + ')' + name.slice(dot);
    used['$' + n] = true;
    return n;
  }

  function human(n) {
    var units = ['B', 'KB', 'MB', 'GB'], i = 0;
    while (n >= 1024 && i < 3) {
      n /= 1024;
      i++;
    }
    return i ? n.toFixed(1) + ' ' + units[i] : n + ' B';
  }

  return {
    tar: tar, tarSlice: tarSlice, estimate: estimate, stream: stream, rawFrame: rawFrame,
    instantiate: instantiate, bundle: bundle, uniq: uniq, human: human
  };
})();

if (typeof document !== 'undefined') (function () {
  'use strict';
  var probe = window.XMLHttpRequest && new XMLHttpRequest();
  if (!(probe && 'upload' in probe && window.Blob && window.FileReader && window.Uint8Array && window.JSON)) return;

  function $(id) {
    return document.getElementById(id);
  }
  function trim(s) {
    return s.replace(/^\s+|\s+$/g, '');
  }
  var form = $('form'), out = $('out'), hoursInput = $('hours'), zstd = null, queue = [], busy = false;

  if (typeof WebAssembly === 'object') {
    var w = new XMLHttpRequest();
    w.open('GET', '/zstd.wasm');
    w.responseType = 'arraybuffer';
    w.onload = function () {
      if (w.status === 200) DTLK.instantiate(w.response, function (z) { zstd = z; });
    };
    w.send();
  }

  // ---- talking to /api ----

  // cb(err, json); err = {status, message}
  function send(method, url, body, cb, onprogress) {
    var x = new XMLHttpRequest();
    x.open(method, url);
    if (onprogress) x.upload.onprogress = onprogress; // must be set before send()
    if (body instanceof Blob) {
      x.setRequestHeader('Content-Type', 'application/octet-stream');
    } else if (body !== null) {
      x.setRequestHeader('Content-Type', 'application/json');
      body = JSON.stringify(body);
    }
    x.onload = function () {
      var j = null;
      try {
        j = JSON.parse(x.responseText);
      } catch (e) {}
      if (x.status >= 200 && x.status < 300) return cb(null, j);
      cb({status: x.status, message: j && typeof j.detail === 'string' ? j.detail : 'Server returned ' + x.status});
    };
    x.onerror = function () {
      cb({status: 0, message: 'Cannot reach the server'});
    };
    x.send(body);
    return x;
  }

  function read(blob, cb) {
    var r = new FileReader();
    r.onload = function () {
      cb(null, new Uint8Array(r.result));
    };
    r.onerror = function () {
      cb({message: 'Cannot read the files (' + (r.error && r.error.name) + ')'});
    };
    r.readAsArrayBuffer(blob);
  }

  // Uploads job, 3 parts at a time, retrying parts like the CLI does.
  // progress(fractionDone, bytesSent); done(err, result, storedSize). Returns cancel().
  function run(job, progress, done) {
    var t = DTLK.tar(job.entries, new Date()), xhrs = [], id = null, over = false;
    var inflight = 0, waiting = null, ended = false, got = 0, made = 0, sent = 0, loaded = {};
    function fail(err) {
      if (over) return;
      over = true;
      for (var i = 0; i < xhrs.length; i++) xhrs[i].abort();
      if (id) send('DELETE', '/api/uploads/' + id, null, function () {});
      done(err);
    }
    // Reading runs ahead of sending, so: share of the tar read x share of its output sent
    function show() {
      var s = sent;
      for (var k in loaded) s += loaded[k];
      progress(made ? (got / t.size) * Math.min(s / made, 1) : 0, s);
    }
    function put(n, blob, attempt) {
      var x = send('PUT', '/api/uploads/' + id + '/parts/' + n, blob, function (err) {
        xhrs.splice(xhrs.indexOf(x), 1);
        delete loaded[n];
        if (over) return;
        if (err) {
          if (attempt < 5 && (err.status === 0 || err.status === 408 || (err.status >= 500 && err.status !== 507))) {
            return setTimeout(function () {
              if (!over) put(n, blob, attempt + 1);
            }, Math.min(1000 * Math.pow(2, attempt), 15000));
          }
          return fail(err);
        }
        sent += blob.size;
        inflight--;
        show();
        if (waiting) {
          var next = waiting;
          waiting = null;
          next();
        } else if (ended && !inflight) {
          finish();
        }
      }, function (e) {
        loaded[n] = e.loaded;
        show();
      });
      xhrs.push(x);
    }
    function finish() {
      send('POST', '/api/uploads/' + id + '/complete', null, function (err, res) {
        if (err) return fail(err);
        over = true;
        done(null, res, sent);
      });
    }
    var body = {name: job.name, kind: job.kind, estimated_size: DTLK.estimate(t.size), hours: job.hours};
    if (job.tag) body.tag = job.tag;
    send('POST', '/api/uploads', body, function (err, up) {
      if (err) return fail(err);
      if (over) return send('DELETE', '/api/uploads/' + up.upload_id, null, function () {});
      id = up.upload_id;
      DTLK.stream(t, job.zstd, up.chunk_size, read, function (n, blob, pos, next) {
        if (over) return;
        got = pos;
        made += blob.size;
        inflight++;
        put(n, blob, 1);
        if (inflight < 3) next();
        else waiting = next;
      }, function () {
        ended = true;
        if (!inflight) finish();
      }, fail);
    });
    return function () {
      fail({message: 'Canceled'});
    };
  }

  // ---- the page ----

  function say(line, text) {
    line.innerHTML = '';
    line.appendChild(document.createTextNode(text));
  }

  function label(job) {
    return job.name + (job.kind === 'dir' ? '/' : '') + ' (' + DTLK.human(job.size) + ')';
  }

  function localTime(iso) {
    var d = new Date(iso);
    function p(n) {
      return (n < 10 ? '0' : '') + n;
    }
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  function result(job, res, stored) {
    var origin = location.protocol + '//' + location.host, table = document.createElement('table');
    table.setAttribute('border', '1');
    table.setAttribute('cellpadding', '6');
    table.setAttribute('cellspacing', '0');
    function row(name, value, href) {
      var tr = table.insertRow(-1), th = document.createElement('th'), td = tr.insertCell(-1);
      tr.insertBefore(th, td);
      th.setAttribute('align', 'left');
      th.appendChild(document.createTextNode(name));
      var node = document.createTextNode(value);
      if (name === 'Code') {
        var b = document.createElement('b');
        b.appendChild(node);
        node = b;
      }
      if (href) {
        var a = document.createElement('a');
        a.href = href;
        a.appendChild(node);
        node = a;
      }
      td.appendChild(node);
    }
    row('Name', job.name + (job.kind === 'dir' ? '/' : ''));
    row('Code', res.code);
    row('Expires', localTime(res.expires_at));
    row('Size', DTLK.human(job.size) + ' \u2192 ' + DTLK.human(stored) + (job.zstd ? ' (zstd)' : ' (not compressed)'));
    row('Web', origin + '/' + res.code, '/' + res.code);
    row('CLI', 'dtlk down ' + res.code);
    if (job.tag) row('List', origin + '/files/' + job.tag, '/files/' + job.tag);
    out.replaceChild(table, job.line);
    out.insertBefore(document.createElement('p'), table.nextSibling);
  }

  function next() {
    if (busy || !queue.length) return;
    busy = true;
    var job = queue.shift(), line = job.line, bar = document.createElement('progress');
    job.zstd = zstd; // null until zstd.wasm has loaded, or if it cannot
    var text = document.createTextNode(''), stop = document.createElement('button');
    stop.type = 'button';
    stop.appendChild(document.createTextNode('Cancel'));
    line.innerHTML = '';
    line.appendChild(text);
    line.appendChild(document.createTextNode(' '));
    line.appendChild(bar);
    line.appendChild(document.createTextNode(' '));
    line.appendChild(stop);
    text.nodeValue = label(job) + ': starting';
    stop.onclick = run(job, function (fraction, sent) {
      bar.max = 100;
      bar.value = Math.floor(100 * fraction);
      text.nodeValue = label(job) + ': ' + bar.value + '% (' + DTLK.human(sent) + ' sent)';
    }, function (err, res, stored) {
      busy = false;
      if (err) say(line, label(job) + ': ' + err.message);
      else result(job, res, stored);
      next();
    });
  }

  // Checks tag and hours as they are now, before the files arrive.
  function newJob() {
    var line = document.createElement('p'), tag = trim($('tag').value), hours = trim(hoursInput.value);
    var max = +hoursInput.getAttribute('max');
    out.appendChild(line);
    if (!/^[A-Za-z0-9_-]{0,64}$/.test(tag)) {
      say(line, 'Tag must be 1-64 letters, digits, - or _');
    } else if (!/^[0-9]+$/.test(hours) || +hours < 1 || +hours > max) {
      say(line, 'Hours must be an integer between 1 and ' + max);
    } else {
      say(line, 'Reading files...');
      return {line: line, tag: tag.toLowerCase(), hours: +hours};
    }
    return null;
  }

  function add(job, items, roots) {
    if (!items.length) return say(job.line, 'Nothing to upload');
    var b = DTLK.bundle(items, roots, new Date());
    job.name = b.name;
    job.kind = b.kind;
    job.entries = b.entries;
    job.size = b.size;
    say(job.line, label(job) + ': waiting');
    queue.push(job);
    next();
  }

  function addFiles(job, files) {
    var used = {}, items = [], roots = [];
    for (var i = 0; i < files.length; i++) {
      var n = DTLK.uniq(files[i].name, used, false);
      items.push({path: n, file: files[i]});
      roots.push(n);
    }
    add(job, items, roots);
  }

  // <input webkitdirectory> lists files only, as "top/sub/a.txt"
  function addFolder(job, files) {
    var seen = {}, items = [];
    for (var i = 0; i < files.length; i++) {
      var parts = (files[i].webkitRelativePath || files[i].name).split('/');
      for (var j = 1; j < parts.length; j++) {
        var dir = parts.slice(0, j).join('/');
        if (!seen['$' + dir]) {
          seen['$' + dir] = true;
          items.push({path: dir, dir: true});
        }
      }
      items.push({path: parts.join('/'), file: files[i]});
    }
    add(job, items, items.length ? [items[0].path.split('/')[0]] : []);
  }

  function each(list, fn, done) {
    var i = 0;
    (function step(err) {
      if (err || i >= list.length) return done(err);
      fn(list[i++], step);
    })();
  }

  function walk(entry, path, items, done) {
    if (entry.isFile) {
      return entry.file(function (f) {
        items.push({path: path, file: f});
        done();
      }, done);
    }
    items.push({path: path, dir: true});
    var reader = entry.createReader(), kids = [];
    (function more() {
      reader.readEntries(function (batch) {
        if (!batch.length) {
          return each(kids, function (k, cb) {
            walk(k, path + '/' + k.name, items, cb);
          }, done);
        }
        for (var i = 0; i < batch.length; i++) kids.push(batch[i]);
        more();
      }, done);
    })();
  }

  function dropped(dt) {
    var list = dt.items, entries = [];
    if (list && list.length && list[0].webkitGetAsEntry) {
      for (var i = 0; i < list.length; i++) {
        var e = list[i].kind === 'file' && list[i].webkitGetAsEntry();
        if (e) entries.push(e);
      }
      if (!entries.length) return;
    } else if (!dt.files || !dt.files.length) {
      return;
    }
    var job = newJob();
    if (!job) return;
    if (!entries.length) return addFiles(job, dt.files);
    var used = {}, items = [], roots = [];
    each(entries, function (e, cb) {
      var n = DTLK.uniq(e.name, used, e.isDirectory);
      roots.push(n);
      walk(e, n, items, cb);
    }, function (err) {
      if (err) return say(job.line, 'Cannot read the files (' + (err.message || err.name) + ')');
      add(job, items, roots);
    });
  }

  function picker(input, addTo) {
    input.onchange = function () {
      if (!input.files.length) return;
      var job = newJob();
      if (job) addTo(job, input.files);
      try {
        input.value = '';
      } catch (e) {}
    };
  }

  form.onsubmit = function () {
    return false;
  };
  say($('how'), 'Drop files or folders anywhere on this page, or choose them above. Uploads start right away.');
  picker($('files'), addFiles);
  var folder = document.createElement('input');
  folder.type = 'file';
  if ('webkitdirectory' in folder) {
    folder.webkitdirectory = true;
    $('folder').appendChild(document.createTextNode(' Folder: '));
    $('folder').appendChild(folder);
    picker(folder, addFolder);
  }
  function hold(e) {
    e.preventDefault();
  }
  document.addEventListener('dragenter', hold, false);
  document.addEventListener('dragover', hold, false);
  document.addEventListener('drop', function (e) {
    e.preventDefault();
    dropped(e.dataTransfer);
  }, false);
  window.addEventListener('beforeunload', function (e) {
    if (busy || queue.length) {
      e.preventDefault();
      e.returnValue = '';
      return '';
    }
  }, false);
})();
