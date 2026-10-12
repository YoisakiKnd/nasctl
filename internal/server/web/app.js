/* albumctl frontend.
 *
 * Constraints this file is built around:
 *   - No network access to anything but this origin. No map tiles, no fonts,
 *     no analytics, no external API. The page works entirely from the API.
 *   - Every value that came from a file name or a tag is inserted with
 *     textContent, never innerHTML. The server sends names as plain JSON
 *     strings, so a name is inert data here and stays that way: nothing in
 *     this file parses HTML, and a name is only ever assigned to textContent.
 *   - The grid is paged and capped. Scrolling appends at most BATCH frames at a
 *     time and stops at MAX_FRAMES until the reader asks for more, so a large
 *     library never turns into thousands of live image elements.
 *   - Nothing here starts work. The page cannot ask the server to generate a
 *     thumbnail, run a scan or bypass a pause; there is no such endpoint.
 */
'use strict';

var PAGE = 60;
var MAX_FRAMES = 600;

var state = {
  collection: '',
  kind: '',
  q: '',
  sort: 'time_desc',
  from: '',
  to: '',
  gps: false,
  fallback: false,
  cursor: null,
  items: [],
  frames: 0,
  loading: false,
  exhausted: false,
  selected: -1,
  auto: true
};

var el = {};
['label-sub','gauge','gauge-value','gauge-bar','gauge-fill','gauge-legend','state-chip','state-text',
 'notice','f-collection','f-q','f-dates','f-gps','f-fallback','f-sort','dates','dates-body',
 'dates-clear','grid','empty','empty-lead','empty-hint','more','more-btn','more-auto','sentinel',
 'card','card-title','card-close','card-media','card-facts','card-extra','card-raw','card-dl',
 'card-next','colophon-count','tick-start','tick-warn','tick-stop'].forEach(function (id) {
  el[id] = document.getElementById(id);
});

/* ---------- fetch helpers ------------------------------------------------ */

function api(path, params) {
  var u = new URL(path, window.location.origin);
  if (params) {
    Object.keys(params).forEach(function (k) {
      var v = params[k];
      if (v !== '' && v !== null && v !== undefined && v !== false) {
        u.searchParams.set(k, v === true ? '1' : String(v));
      }
    });
  }
  return fetch(u.toString(), {
    headers: { 'Accept': 'application/json' },
    credentials: 'same-origin',
    cache: 'no-store'
  });
}

function humanBytes(n) {
  if (n === null || n === undefined) { return '—'; }
  if (n < 1024) { return n + ' B'; }
  var units = ['KiB', 'MiB', 'GiB', 'TiB'], v = n, i = -1;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (v < 10 ? v.toFixed(1) : Math.round(v)) + ' ' + units[i];
}

function humanDuration(ms) {
  if (!ms) { return null; }
  var s = Math.round(ms / 1000);
  var m = Math.floor(s / 60), r = s % 60;
  var h = Math.floor(m / 60);
  m = m % 60;
  if (h > 0) { return h + ':' + String(m).padStart(2, '0') + ':' + String(r).padStart(2, '0'); }
  return m + ':' + String(r).padStart(2, '0');
}

/* taken_at arrives as a naive ISO string. It is displayed exactly as stored:
   for a camera with no offset those digits are the wall clock the camera
   showed, and shifting them into the browser's zone would silently rewrite the
   past. */
function displayTaken(v) {
  if (!v) { return null; }
  return v.replace('T', ' ').slice(0, 16);
}

/* ---------- status ------------------------------------------------------ */

function renderStatus(s) {
  var parts = [];
  parts.push(s.totals ? s.totals.media.toLocaleString() : '0');
  parts.push('frames');
  if (s.totals && s.totals.images) { parts.push(s.totals.images.toLocaleString() + ' stills'); }
  if (s.totals && s.totals.videos) { parts.push(s.totals.videos.toLocaleString() + ' moving'); }
  if (s.collections && s.collections.length) { parts.push(s.collections.length + ' album' + (s.collections.length === 1 ? '' : 's')); }
  if (s.totals && s.totals.thumb_pending) { parts.push(s.totals.thumb_pending.toLocaleString() + ' awaiting a thumbnail'); }
  el['label-sub'].textContent = parts.join(' · ');

  if (s.totals) {
    var c = s.totals.media + ' frames indexed';
    if (s.totals.undated) { c += ', ' + s.totals.undated + ' without a usable date'; }
    if (s.totals.missing) { c += ', ' + s.totals.missing + ' recorded but not present on disk'; }
    el['colophon-count'].textContent = c;
  }

  var chip = el['state-chip'], text = el['state-text'];
  if (s.paused) {
    chip.dataset.state = 'paused';
    text.textContent = 'paused';
  } else if (s.resource && s.resource.pressure_tripped) {
    chip.dataset.state = 'tripped';
    text.textContent = 'stopped by pressure';
  } else if (s.running) {
    chip.dataset.state = 'running';
    text.textContent = (s.running.kind || 'task') + ' running';
  } else {
    chip.dataset.state = 'ok';
    text.textContent = 'idle';
  }
  chip.title = s.paused
    ? ('Paused: ' + (s.pause_reason || 'no reason recorded'))
    : (s.running ? 'A task is running. This page cannot stop it.'
                 : 'No task is running. Browsing never starts one.');

  renderGauge(s.resource);
  renderNotices(s);
}

function renderGauge(r) {
  if (!r || (!r.mem_available_kb && !r.mem_total_kb)) {
    el.gauge.hidden = true;
    return;
  }
  el.gauge.hidden = false;
  var total = r.mem_total_kb || 1024 * 1024;
  var avail = r.mem_available_kb || 0;
  var pct = Math.max(0, Math.min(100, (avail / total) * 100));
  var fill = el['gauge-fill'];
  fill.style.width = pct.toFixed(1) + '%';

  var band = 'ok';
  if (avail <= (r.stop_threshold_kb || 0)) { band = 'stop'; }
  else if (avail <= (r.warn_threshold_kb || 0)) { band = 'warn'; }
  fill.style.background = band === 'stop' ? 'var(--stop)'
                        : band === 'warn' ? 'var(--warn)' : 'var(--ok)';

  el['gauge-value'].textContent = (avail / 1024).toFixed(0) + ' MiB free of '
    + (total / 1024).toFixed(0) + ' MiB';

  var ticks = [['tick-start', r.start_threshold_kb], ['tick-warn', r.warn_threshold_kb], ['tick-stop', r.stop_threshold_kb]];
  ticks.forEach(function (t) {
    var node = el[t[0]];
    if (!node) { return; }
    if (!t[1]) { node.hidden = true; return; }
    node.hidden = false;
    node.style.left = Math.min(100, (t[1] / total) * 100).toFixed(2) + '%';
  });
  el['gauge-legend'].hidden = !r.start_threshold_kb;
  el['gauge-bar'].setAttribute('aria-label',
    'available memory ' + (avail / 1024).toFixed(0) + ' of ' + (total / 1024).toFixed(0) + ' mebibytes');
}

function showNotice(level, text, actionLabel, action) {
  el.notice.hidden = false;
  el.notice.dataset.level = level;
  el.notice.textContent = text;
  if (actionLabel && action) {
    var b = document.createElement('button');
    b.type = 'button';
    b.textContent = actionLabel;
    b.addEventListener('click', action);
    el.notice.appendChild(b);
  }
}

function renderNotices(s) {
  var w = s.warnings || [];
  if (s.paused) {
    showNotice('warn',
      'Browsing is read-only and still works. Thumbnail and scan runs will not start until albumctl is resumed on the machine.',
      'Show details', function () { showNoticeDetail(s); });
    return;
  }
  if (s.resource && s.resource.pressure_tripped) {
    showNotice('stop',
      'A previous run was stopped by resource pressure, so heavy tasks stay disabled. Existing thumbnails keep loading; frames without one show a placeholder.',
      'Show details', function () { showNoticeDetail(s); });
    return;
  }
  if (w.length) {
    showNotice('ok', w[0], w.length > 1 ? 'More' : null,
      w.length > 1 ? function () { showNotice('ok', w.slice(1).join(' ')); } : null);
    return;
  }
  el.notice.hidden = true;
  el.notice.textContent = '';
}

function showNoticeDetail(s) {
  var lines = [];
  if (s.pause_reason) { lines.push('Pause reason: ' + s.pause_reason); }
  if (s.pause_since) { lines.push('Paused since ' + new Date(s.pause_since * 1000).toLocaleString()); }
  if (s.resource && s.resource.pressure_tripped) {
    lines.push('Pressure stop recorded at '
      + new Date(s.resource.pressure_tripped_at * 1000).toLocaleString() + '.');
    lines.push('Clear it on the machine with: albumctl resume');
  }
  if (s.last && s.last.detail) { lines.push('Last run: ' + s.last.detail); }
  (s.warnings || []).forEach(function (x) { lines.push(x); });
  showNotice(el.notice.dataset.level || 'warn', lines.join(' '));
}

function pollStatus() {
  api('/api/v1/status').then(function (r) {
    if (!r.ok) { throw new Error('status ' + r.status); }
    return r.json();
  }).then(renderStatus).catch(function () {
    el['state-text'].textContent = 'unreachable';
    el['state-chip'].dataset.state = 'tripped';
    showNotice('stop', 'The album service did not answer. It may have been stopped or the machine may be busy.');
  });
}

/* ---------- collections ------------------------------------------------- */

function loadCollections() {
  api('/api/v1/collections').then(function (r) { return r.ok ? r.json() : null; })
    .then(function (d) {
      if (!d) { return; }
      var sel = el['f-collection'];
      while (sel.options.length > 1) { sel.remove(1); }
      (d.items || []).forEach(function (c) {
        if (!c.enabled) { return; }
        var o = document.createElement('option');
        o.value = c.code;
        o.textContent = c.name + ' — ' + c.media_count.toLocaleString()
          + (c.mount_ok ? '' : ' (source missing)');
        if (!c.mount_ok) { o.title = c.mount_note || 'the expected mount was not present at the last check'; }
        sel.appendChild(o);
      });
    }).catch(function () { /* the status call reports the failure */ });
}

/* ---------- timeline ---------------------------------------------------- */

var MONTH_NAMES = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];

function loadTimeline() {
  api('/api/v1/timeline', { collection: state.collection }).then(function (r) {
    return r.ok ? r.json() : null;
  }).then(function (d) {
    if (!d) { return; }
    var body = el['dates-body'];
    body.textContent = '';
    (d.years || []).forEach(function (y) {
      var wrap = document.createElement('div');
      wrap.className = 'dates__year';
      var b = document.createElement('button');
      b.type = 'button';
      b.appendChild(document.createTextNode(String(y.year) + ' '));
      var n = document.createElement('span');
      n.className = 'n';
      n.textContent = y.count.toLocaleString();
      b.appendChild(n);
      b.setAttribute('aria-pressed', state.from === y.year + '-01-01' ? 'true' : 'false');
      b.addEventListener('click', function () { setRange(y.year + '-01-01', y.year + '-12-31'); });
      wrap.appendChild(b);

      var months = document.createElement('div');
      months.className = 'dates__months';
      (y.months || []).forEach(function (m) {
        var mb = document.createElement('button');
        mb.type = 'button';
        mb.textContent = MONTH_NAMES[m.month - 1] || String(m.month);
        mb.title = m.count.toLocaleString() + ' frames';
        var key = y.year + '-' + String(m.month).padStart(2, '0');
        mb.setAttribute('aria-pressed', state.from === key + '-01' ? 'true' : 'false');
        mb.addEventListener('click', function () {
          var last = new Date(y.year, m.month, 0).getDate();
          setRange(key + '-01', key + '-' + String(last).padStart(2, '0'));
        });
        months.appendChild(mb);
      });
      wrap.appendChild(months);
      body.appendChild(wrap);
    });
    if (d.undated) {
      var u = document.createElement('p');
      u.className = 'dates__undated';
      u.textContent = d.undated.toLocaleString() + ' frames have no usable date';
      body.appendChild(u);
    }
    if (!(d.years || []).length) {
      var p = document.createElement('p');
      p.className = 'dates__undated';
      p.textContent = 'No dated frames yet. Run a scan on the machine to build the index.';
      body.appendChild(p);
    }
  }).catch(function () {});
}

function setRange(from, to) {
  state.from = from === state.from && to === state.to ? '' : from;
  state.to   = state.from ? to : '';
  reset(true);
  loadTimeline();
}

/* ---------- frames ------------------------------------------------------ */

function frameNode(item, index) {
  var fig = document.createElement('button');
  fig.type = 'button';
  fig.className = 'frame';
  fig.dataset.index = String(index);

  var box = document.createElement('div');
  box.className = 'frame__box';

  var img = document.createElement('img');
  img.className = 'frame__img';
  img.loading = 'lazy';
  img.decoding = 'async';
  img.alt = item.name;
  img.src = item.thumb_url;
  img.addEventListener('error', function () { img.style.visibility = 'hidden'; });
  box.appendChild(img);

  if (item.kind === 'video') {
    var g = document.createElement('span');
    g.className = 'frame__glyph';
    var d = humanDuration(item.duration_ms);
    g.textContent = d ? d + (item.playable ? ' ▶' : ' ↓') : 'video ↓';
    box.appendChild(g);
  }
  if (item.thumb_state !== 'ok') {
    var p = document.createElement('span');
    p.className = 'frame__pill';
    p.dataset.s = item.thumb_state;
    p.textContent = item.thumb_state === 'failed' ? 'no preview'
                  : item.thumb_state === 'skipped' ? 'skipped' : 'queued';
    p.title = item.note || 'no thumbnail has been generated for this frame yet';
    box.appendChild(p);
  }

  var strip = document.createElement('span');
  strip.className = 'frame__strip';
  var name = document.createElement('span');
  name.className = 'frame__name';
  name.textContent = item.name;
  strip.appendChild(name);
  var meta = document.createElement('span');
  meta.className = 'frame__meta';
  var t = displayTaken(item.taken_at);
  if (t) {
    if (item.taken_is_fallback) {
      var fb = document.createElement('span');
      fb.className = 'fallback';
      fb.textContent = t + ' ~';
      fb.title = 'Approximate: ' + item.taken_label;
      meta.appendChild(fb);
    } else {
      meta.appendChild(document.createTextNode(t));
    }
  } else {
    meta.appendChild(document.createTextNode('no date'));
  }
  if (item.width && item.height) {
    meta.appendChild(document.createTextNode(' · ' + item.width + '×' + item.height));
  }
  strip.appendChild(meta);

  fig.appendChild(box);
  fig.appendChild(strip);
  fig.addEventListener('click', function () { openCard(index); });
  return fig;
}

function appendItems(items) {
  var frag = document.createDocumentFragment();
  var base = state.items.length;
  items.forEach(function (it, i) {
    state.items.push(it);
    frag.appendChild(frameNode(it, base + i));
  });
  el.grid.appendChild(frag);
  state.frames = state.items.length;
}

function loadNext(manual) {
  if (state.loading || state.exhausted) { return; }
  if (!manual && state.frames >= MAX_FRAMES) {
    setMore('pause', 'Paused after ' + MAX_FRAMES + ' frames so the page stays light. Narrow the filters, or load more.');
    return;
  }
  state.loading = true;
  if (state.frames === 0) {
    el['empty'].hidden = true;
  }
  setMore('loading', 'loading…');

  api('/api/v1/media', {
    collection: state.collection, kind: state.kind, q: state.q, sort: state.sort,
    from: state.from, to: state.to,
    gps: state.gps, fallback: state.fallback,
    limit: PAGE, cursor: state.cursor
  }).then(function (r) {
    if (!r.ok) { return r.json().then(function (e) { throw new Error((e.error && e.error.message) || ('HTTP ' + r.status)); }); }
    return r.json();
  }).then(function (d) {
    appendItems(d.items || []);
    state.cursor = d.next_cursor;
    state.exhausted = !d.next_cursor;
    state.loading = false;
    if (state.items.length === 0) {
      el['empty'].hidden = false;
      el['empty-lead'].textContent = 'No frames match.';
      el['empty-hint'].textContent = state.q
        ? 'The search covers file names and folder paths. Try fewer characters.'
        : 'Try clearing the date range or the album filter.';
      setMore('none');
    } else if (state.exhausted) {
      setMore('none');
    } else if (state.frames >= MAX_FRAMES) {
      setMore('pause', 'Paused after ' + MAX_FRAMES + ' frames so the page stays light. Narrow the filters, or load more.');
    } else {
      setMore('idle');
    }
    if (d.degraded || d.paused) { pollStatus(); }
  }).catch(function (err) {
    state.loading = false;
    setMore('error', 'Could not load frames: ' + err.message);
  });
}

function setMore(mode, text) {
  if (mode === 'loading') {
    el['more-btn'].hidden = true;
    el['more-auto'].hidden = false;
    el['more-auto'].textContent = text || 'loading…';
    return;
  }
  if (mode === 'none') {
    el['more-btn'].hidden = true;
    el['more-auto'].hidden = true;
    return;
  }
  if (mode === 'pause' || mode === 'error') {
    el['more-btn'].hidden = false;
    el['more-btn'].textContent = mode === 'error' ? 'Try again' : 'Load more frames';
    el['more-auto'].hidden = false;
    el['more-auto'].textContent = text || '';
    return;
  }
  el['more-btn'].hidden = false;
  el['more-btn'].textContent = 'Load more frames';
  el['more-auto'].hidden = true;
}

function reset(refill) {
  state.cursor = null;
  state.items = [];
  state.frames = 0;
  state.exhausted = false;
  state.selected = -1;
  el.grid.textContent = '';
  el['card'].hidden = true;
  el['empty'].hidden = true;
  if (refill) { loadNext(false); }
}

/* ---------- detail card ------------------------------------------------- */

function fact(dl, label, value, fallback) {
  if (value === null || value === undefined || value === '') { return; }
  var dt = document.createElement('dt');
  dt.textContent = label;
  var dd = document.createElement('dd');
  dd.textContent = value;
  if (fallback) {
    dd.className = 'fallback';
    dd.title = 'Fallback value, not a real capture time';
  }
  dl.appendChild(dt);
  dl.appendChild(dd);
}

function openCard(index) {
  var it = state.items[index];
  if (!it) { return; }
  state.selected = index;
  el['card'].hidden = false;
  el['card-title'].textContent = it.name;
  el['card-raw'].href = it.raw_url;
  el['card-dl'].href = '/api/v1/media/' + it.id + '/download';
  el['card-next'].hidden = index + 1 >= state.items.length;
  el['card-next'].href = '#';
  el['card-next'].onclick = function (e) { e.preventDefault(); openCard(index + 1); };

  Array.prototype.forEach.call(el.grid.children, function (c) {
    if (c.dataset.index === String(index)) { c.setAttribute('aria-current', 'true'); }
    else { c.removeAttribute('aria-current'); }
  });

  var media = el['card-media'];
  media.textContent = '';
  if (it.kind === 'video') {
    if (it.playable) {
      var v = document.createElement('video');
      v.controls = true;
      v.preload = 'metadata';
      v.playsInline = true;
      v.src = it.raw_url;
      media.appendChild(v);
    } else {
      var u = document.createElement('p');
      u.className = 'unsupported';
      u.textContent = 'This container is not played by browsers, and albumctl does not transcode. '
        + 'Open or download the original and play it locally.';
      media.appendChild(u);
    }
  } else if (it.thumb_state === 'ok') {
    var big = document.createElement('img');
    big.src = it.thumb_url;
    big.alt = it.name;
    big.decoding = 'async';
    media.appendChild(big);
  } else {
    var ph = document.createElement('img');
    ph.src = it.thumb_url;
    ph.alt = 'no thumbnail yet';
    ph.decoding = 'async';
    media.appendChild(ph);
  }

  var dl = el['card-facts'];
  dl.textContent = '';
  fact(dl, 'Album', it.collection);
  fact(dl, 'Folder', it.rel_dir ? it.rel_dir : '/');
  fact(dl, 'Size', humanBytes(it.bytes));
  fact(dl, 'Dimensions', it.width && it.height ? it.width + ' × ' + it.height : 'not read yet');
  var taken = displayTaken(it.taken_at);
  fact(dl, 'Taken', taken ? taken + '  (' + it.taken_label + ')' : 'no usable capture time',
       it.taken_is_fallback);
  if (it.taken_tz === 'naive') {
    fact(dl, 'Time zone', 'as recorded by the camera; no shifting applied');
  } else if (it.tz_offset_min !== null && it.tz_offset_min !== undefined) {
    var sign = it.tz_offset_min < 0 ? '-' : '+';
    var abs = Math.abs(it.tz_offset_min);
    fact(dl, 'Time zone', 'UTC' + sign + String(Math.floor(abs / 60)).padStart(2, '0')
      + ':' + String(abs % 60).padStart(2, '0'));
  }
  fact(dl, 'Camera', [it.make, it.model].filter(Boolean).join(' ') || null);
  if (it.orientation > 1) { fact(dl, 'Orientation', 'tag ' + it.orientation); }
  if (it.duration_ms) { fact(dl, 'Duration', humanDuration(it.duration_ms)); }
  if (it.gps) {
    fact(dl, 'Coordinates', it.gps.lat.toFixed(6) + ', ' + it.gps.lon.toFixed(6));
  } else {
    fact(dl, 'Coordinates', 'none recorded');
  }
  fact(dl, 'Probe', it.probe_state);
  fact(dl, 'Thumbnail', it.thumb_state);

  var extra = el['card-extra'];
  extra.textContent = '';
  var unknown = [];
  if (!it.width || !it.height) { unknown.push('dimensions have not been read'); }
  if (!it.taken_at) { unknown.push('no capture time was found in the file or its name'); }
  if (!it.gps) { unknown.push('no GPS tag'); }
  if (unknown.length || it.missing || it.note) {
    var d = document.createElement('details');
    var sm = document.createElement('summary');
    sm.textContent = 'What is missing or unusual';
    d.appendChild(sm);
    var ul = document.createElement('ul');
    if (it.missing) { unknown.unshift('the file was not present during the last complete scan'); }
    if (it.note) { unknown.unshift(it.note); }
    unknown.forEach(function (x) {
      var li = document.createElement('li');
      li.textContent = x;
      ul.appendChild(li);
    });
    d.appendChild(ul);
    extra.appendChild(d);
  }
  loadItemDetail(it.id, extra);

  el['card'].scrollTop = 0;
  if (window.matchMedia('(max-width: 1000px)').matches) {
    el['card'].scrollIntoView({ block: 'nearest' });
  }
}

/* The list endpoint carries everything the card needs. The per-item endpoint is
   fetched only when the card is open, and only for the extra tags, so a gallery
   scroll never issues one request per frame. */
function loadItemDetail(id, host) {
  api('/api/v1/media/' + id).then(function (r) { return r.ok ? r.json() : null; })
    .then(function (d) {
      if (!d || !d.exif || !Object.keys(d.exif).length) { return; }
      var box = document.createElement('details');
      var sm = document.createElement('summary');
      sm.textContent = 'Other tags read from the file';
      box.appendChild(sm);
      var dl = document.createElement('dl');
      dl.className = 'card__facts';
      Object.keys(d.exif).slice(0, 24).forEach(function (k) {
        fact(dl, k, String(d.exif[k]).slice(0, 200));
      });
      box.appendChild(dl);
      host.appendChild(box);
    }).catch(function () {});
}

function closeCard() {
  el['card'].hidden = true;
  state.selected = -1;
  Array.prototype.forEach.call(el.grid.children, function (c) { c.removeAttribute('aria-current'); });
}

/* ---------- wiring ------------------------------------------------------ */

function debounce(fn, ms) {
  var t = null;
  return function () {
    var args = arguments;
    clearTimeout(t);
    t = setTimeout(function () { fn.apply(null, args); }, ms);
  };
}

function wire() {
  el['f-collection'].addEventListener('change', function (e) {
    state.collection = e.target.value;
    reset(true);
    loadTimeline();
  });

  Array.prototype.forEach.call(document.querySelectorAll('.seg[data-kind]'), function (b) {
    b.addEventListener('click', function () {
      Array.prototype.forEach.call(document.querySelectorAll('.seg[data-kind]'), function (o) {
        o.setAttribute('aria-pressed', o === b ? 'true' : 'false');
      });
      state.kind = b.dataset.kind;
      reset(true);
    });
  });

  el['f-q'].addEventListener('input', debounce(function (e) {
    state.q = e.target.value.trim();
    reset(true);
  }, 300));

  el['f-sort'].addEventListener('change', function (e) {
    state.sort = e.target.value;
    reset(true);
  });

  el['f-gps'].addEventListener('click', function () {
    state.gps = !state.gps;
    el['f-gps'].setAttribute('aria-pressed', state.gps ? 'true' : 'false');
    reset(true);
  });

  el['f-fallback'].addEventListener('click', function () {
    state.fallback = !state.fallback;
    el['f-fallback'].setAttribute('aria-pressed', state.fallback ? 'true' : 'false');
    reset(true);
  });

  el['f-dates'].addEventListener('click', function () {
    var open = el['dates'].hidden;
    el['dates'].hidden = !open;
    el['f-dates'].setAttribute('aria-expanded', open ? 'true' : 'false');
  });

  el['dates-clear'].addEventListener('click', function () {
    state.from = '';
    state.to = '';
    reset(true);
    loadTimeline();
  });

  el['card-close'].addEventListener('click', closeCard);
  el['more-btn'].addEventListener('click', function () { loadNext(true); });

  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') {
      if (!el['card'].hidden) { closeCard(); }
      else if (!el['dates'].hidden) { el['dates'].hidden = true; el['f-dates'].setAttribute('aria-expanded', 'false'); }
      return;
    }
    if (el['card'].hidden && !el['dates'].hidden) { return; }
    if (e.key === 'ArrowRight' && state.selected >= 0 && state.selected + 1 < state.items.length) {
      e.preventDefault(); openCard(state.selected + 1);
    }
    if (e.key === 'ArrowLeft' && state.selected > 0) {
      e.preventDefault(); openCard(state.selected - 1);
    }
  });

  if ('IntersectionObserver' in window) {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (en.isIntersecting && !state.loading && !state.exhausted) { loadNext(false); }
      });
    }, { rootMargin: '600px 0px' });
    io.observe(el.sentinel);
  }
}

wire();
pollStatus();
loadCollections();
loadTimeline();
loadNext(false);
setInterval(pollStatus, 30000);