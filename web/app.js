// ClickHouse Viewer — фронтенд: MapLibre GL (WebGL), слои полигонов из ClickHouse
// и сетка H3 из компонента react-h3-map (используем его не-React части).
import * as maplibregl from 'maplibre-gl';
import 'maplibre-gl/dist/maplibre-gl.css';
import { latLngToCell } from 'h3-js';
import { reprojectedTiles } from './merc3395.js';
// сетка H3 — компонент react-h3-map, подключён git-submodule'ом
// (https://github.com/akprof2000/Demo-H3-Hex), используем его не-React части
import { H3GridLayer } from './vendor/Demo-H3-Hex/src/H3GridLayer.ts';
import {
  buildGridMesh, cellsForBBox, estimateCellCount,
  resolutionForEdgePixels, padBBox
} from './vendor/Demo-H3-Hex/src/gridGeometry.ts';

// Все POST к локальному бэку идут как application/json. Это не украшение:
// сервер отклоняет изменяющие запросы с другим Content-Type, а простая
// HTML-форма со стороннего сайта такой заголовок поставить не может —
// значит, CSRF на localhost:8137 невозможен.
const JSON_HDR = { 'Content-Type': 'application/json' };

const $ = id => document.getElementById(id);
const statusEl = $('status'), msg = $('msg');
const MAX_ROWS = 30000;         // предохранитель на один запрос к БД
const MIN_OBJ_PX = 3;           // объекты мельче стольких пикселей не грузим и не рисуем
const MAX_GRID_CELLS = 60000;   // предохранитель сетки H3
const PALETTE = ['#1565c0', '#c62828', '#2e7d32', '#ef6c00', '#6a1b9a', '#00838f', '#ad1457', '#558b2f'];
let lastSQL = '';

function say(t, err) { $('statustext').textContent = t; statusEl.className = err ? 'err' : ''; }

function showSqlBox(errText) {
  $('sqltext').textContent = lastSQL || '(запрос ещё не выполнялся)';
  const e = $('sqlerr');
  if (errText) { e.textContent = errText; e.style.display = ''; }
  else e.style.display = 'none';
  $('sqlbox').classList.add('show');
}

// ---------- карта (WebGL) ----------
// maplibre 6 ищет свой воркер по import.meta.url, который при сборке в IIFE
// пустой: получается new Worker('', {type:'module'}) -> запрос самой страницы
// -> воркер не стартует и GeoJSON-слои молча не рисуются. Поэтому собираем
// воркер отдельным самодостаточным файлом (см. web/package.json) и указываем
// его адрес явно. Вызов обязан идти до создания карты.
maplibregl.setWorkerUrl(new URL('maplibre-gl-worker.mjs', document.baseURI).href);

// ---------- подложки ----------
// Список приходит с сервера: какие источники включены, решает администратор.
let basemaps = [];
let basemapIndex = 0;

function basemapStyle(b) {
  // Тайлы в EPSG:3395 (Яндекс) перепроецируются на лету в 3857, иначе
  // подложка съедет относительно данных — см. merc3395.js
  const tiles = String(b.projection) === '3395'
    ? b.tiles.map(t => reprojectedTiles(maplibregl, t))
    : b.tiles;
  return {
    version: 8,
    sources: {
      base: {
        type: 'raster',
        tiles,
        tileSize: b.tile_size || 256,
        maxzoom: b.max_zoom || 19,
        attribution: b.attribution || ''
      }
    },
    layers: [
      { id: 'bg', type: 'background', paint: { 'background-color': '#0d1117' } },
      { id: 'base', type: 'raster', source: 'base' }
    ]
  };
}

// Подложка по умолчанию, пока не пришёл ответ сервера: без неё карту
// нельзя создать, а создаётся она сразу при загрузке страницы.
const FALLBACK_BASEMAP = {
  name: 'OpenStreetMap',
  tiles: ['https://tile.openstreetmap.org/{z}/{x}/{y}.png'],
  attribution: '© OpenStreetMap contributors',
  max_zoom: 19
};

const map = new maplibregl.Map({
  container: 'map',
  style: basemapStyle(FALLBACK_BASEMAP),
  center: [37.62, 55.75],
  zoom: 10,
  attributionControl: { compact: true }
});
map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-left');
let mapReady = false;
map.on('load', () => { mapReady = true; rebuildMapLayers(); loadVisible(); });

const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

function fmtVal(v) {
  // MapLibre строкует вложенные объекты в properties — вернём как было
  if (typeof v === 'string' && (v.startsWith('{') || v.startsWith('['))) {
    try { v = JSON.parse(v); }
    catch (err) { console.debug('fmtVal: значение не JSON, показываю как строку:', err.message); }
  }
  if (v === null || v === undefined || v === '') return '<span class="pv-empty">—</span>';
  if (typeof v === 'boolean') return `<span class="pv-bool">${v ? 'да' : 'нет'}</span>`;
  if (typeof v === 'number') return `<span class="pv-num">${Number.isInteger(v) ? v.toLocaleString('ru-RU') : v}</span>`;
  if (typeof v === 'string') {
    if (/^-?\d+$/.test(v) && v.length < 19) return `<span class="pv-num">${(+v).toLocaleString('ru-RU')}</span>`;
    return esc(v);
  }
  const s = JSON.stringify(v);
  return `<span class="pv-json" title="${esc(s)}">${esc(s.length > 60 ? s.slice(0, 60) + '…' : s)}</span>`;
}

function popupHTML(props, title, color) {
  const entries = Object.entries(props).filter(([k]) => !k.startsWith('__'));
  const head = `<div class="pv-title"><span class="pv-dot" style="background:${color}"></span>${esc(title)}</div>`;
  if (!entries.length) return `<div class="pv">${head}<div class="pv-none">нет атрибутов</div></div>`;
  const rows = entries.map(([k, v]) =>
    `<tr><td class="pv-key">${esc(k)}</td><td class="pv-val">${fmtVal(v)}</td></tr>`).join('');
  return `<div class="pv">${head}<table>${rows}</table></div>`;
}

// ---------- слои ----------
// таблица: {kind:'table', table, geo, color, loaded:Set, features:[], needReset}
// сетка H3: {kind:'grid', enabled, color, resolution:'auto'|число}
let layers = [];
let connected = false;
let debounceTimer = null;
let inFlight = null;
let seq = 0;
let gridLayerObj = null;   // экземпляр H3GridLayer на карте
let gridRes = 9;           // фактическое разрешение последней сетки (для кликов)

function newTableLayer(table, geo, color, visible) {
  const colorIdx = layers.filter(l => l.kind === 'table').length;
  return {
    kind: 'table', id: 'lyr' + (seq++),
    table: table || '', geo: geo || 'key',
    color: color || PALETTE[colorIdx % PALETTE.length],
    visible: visible ?? true,
    loaded: new Set(), features: [], needReset: true
  };
}
function newGridLayer(color, resolution, enabled, width) {
  return {
    kind: 'grid', id: 'h3grid',
    color: color || '#000000', resolution: resolution ?? 'auto',
    enabled: enabled ?? false, width: width || 2
  };
}

const hexToRGBA = (hex, a) => {
  const n = parseInt(hex.slice(1), 16);
  return [((n >> 16) & 255) / 255, ((n >> 8) & 255) / 255, (n & 255) / 255, a];
};

// пересоздать все слои карты по текущему списку (после connect или reorder)
function rebuildMapLayers() {
  if (!mapReady) return;
  // снять все наши слои
  for (const l of layers) {
    if (l.kind === 'table') {
      for (const suf of ['-fill', '-line', '-sel']) {
        if (map.getLayer(l.id + suf)) map.removeLayer(l.id + suf);
      }
      if (map.getSource(l.id)) map.removeSource(l.id);
    }
  }
  if (gridLayerObj && map.getLayer('h3grid')) { map.removeLayer('h3grid'); gridLayerObj = null; }

  // добавить снизу вверх: последний в списке — нижний
  for (let i = layers.length - 1; i >= 0; i--) {
    const l = layers[i];
    if (l.kind === 'table') {
      // generateId — чтобы работал feature-state (подсветка при наведении)
      map.addSource(l.id, { type: 'geojson', generateId: true, data: { type: 'FeatureCollection', features: l.features } });
      const vis = l.visible ? 'visible' : 'none';
      const hover = ['boolean', ['feature-state', 'hover'], false];
      map.addLayer({
        id: l.id + '-fill', type: 'fill', source: l.id,
        layout: { visibility: vis },
        paint: { 'fill-color': l.color, 'fill-opacity': ['case', hover, 0.4, 0.15] }
      });
      map.addLayer({
        id: l.id + '-line', type: 'line', source: l.id,
        layout: { visibility: vis },
        paint: { 'line-color': l.color, 'line-width': ['case', hover, 3, 1.5] }
      });
      // контур выделения найденного объекта (фильтр по __id ставит selectObject)
      map.addLayer({
        id: l.id + '-sel', type: 'line', source: l.id,
        layout: { visibility: vis },
        filter: ['==', ['get', '__id'], ' '],
        paint: { 'line-color': '#f6a800', 'line-width': 4 }
      });
    } else if (l.enabled) {
      gridLayerObj = new H3GridLayer('h3grid', { color: hexToRGBA(l.color, 0.45), width: l.width || 2 });
      map.addLayer(gridLayerObj);
    }
  }
  rebuildGrid();
}

function restyleLayer(l) {
  if (!mapReady) return;
  if (l.kind === 'table') {
    if (map.getLayer(l.id + '-fill')) map.setPaintProperty(l.id + '-fill', 'fill-color', l.color);
    if (map.getLayer(l.id + '-line')) map.setPaintProperty(l.id + '-line', 'line-color', l.color);
  } else if (gridLayerObj) {
    gridLayerObj.setColor(hexToRGBA(l.color, 0.45));
  }
}

function updateCount(extra) {
  const total = layers.reduce((s, l) => s + (l.kind === 'table' ? l.loaded.size : 0), 0);
  say(`Показано объектов: ${total}` + (extra || ''));
}

// ---------- сетка H3 ----------
function gridConf() { return layers.find(l => l.kind === 'grid'); }

function rebuildGrid() {
  const g = gridConf();
  if (!g || !g.enabled || !gridLayerObj || !mapReady) return;
  const b = map.getBounds();
  const bbox = padBBox([b.getWest(), b.getSouth(), b.getEast(), b.getNorth()]);
  const res = g.resolution === 'auto'
    ? resolutionForEdgePixels(map.getZoom(), map.getCenter().lat, 40)
    : +g.resolution;
  gridRes = res;
  if (estimateCellCount(bbox, res) > MAX_GRID_CELLS) {
    gridLayerObj.setMesh(buildGridMesh([], res));
    updateCount(' — сетка H3: слишком мелкое разрешение для этого масштаба');
    return;
  }
  gridLayerObj.setMesh(buildGridMesh(cellsForBBox(bbox, res), res));
}

// какие разрешения сетки доступны на текущем масштабе
function updateGridResOptions() {
  const sel = document.querySelector('.gridrow select');
  if (!sel || !mapReady) return;
  const b = map.getBounds();
  const bbox = padBBox([b.getWest(), b.getSouth(), b.getEast(), b.getNorth()]);
  for (const o of sel.querySelectorAll('option')) {
    if (o.value === 'auto') continue;
    const r = +o.value;
    const ok = estimateCellCount(bbox, r) <= MAX_GRID_CELLS;
    o.disabled = !ok;
    o.textContent = ok ? String(r) : `${r} — приблизьте карту`;
  }
}

// ---------- клики и наведение: работаем с ВЕРХНИМ объектом под мышью ----------
const popup = new maplibregl.Popup({ maxWidth: '420px', className: 'pv-popup' });

// кто сверху под курсором с учётом порядка слоёв:
// {kind:'table', idx, feature} | {kind:'grid', idx} | null
function topObjectAt(point) {
  const fillIds = layers
    .filter(l => l.kind === 'table' && l.visible && map.getLayer(l.id + '-fill'))
    .map(l => l.id + '-fill');
  const feats = fillIds.length ? map.queryRenderedFeatures(point, { layers: fillIds }) : [];
  let best = null, bestIdx = Infinity;
  for (const f of feats) {
    const idx = layers.findIndex(l => l.kind === 'table' && f.layer.id === l.id + '-fill');
    if (idx >= 0 && idx < bestIdx) { bestIdx = idx; best = f; }
  }
  const g = gridConf();
  const gIdx = g && g.enabled && gridLayerObj ? layers.indexOf(g) : Infinity;
  // сетка есть в каждой точке экрана, поэтому сравниваем только позиции слоёв
  if (gIdx < bestIdx) return { kind: 'grid', idx: gIdx };
  if (best) return { kind: 'table', idx: bestIdx, feature: best };
  if (gIdx !== Infinity) return { kind: 'grid', idx: gIdx };
  return null;
}

let hovered = null; // {srcId, fid} — полигон, подсвеченный сейчас

function clearPolyHover() {
  if (hovered) {
    map.setFeatureState({ source: hovered.srcId, id: hovered.fid }, { hover: false });
    hovered = null;
  }
}

map.on('click', e => {
  const top = topObjectAt(e.point);
  if (!top) return;
  if (top.kind === 'table') {
    const l = layers[top.idx];
    popup.setLngLat(e.lngLat).setHTML(popupHTML(top.feature.properties, l.table, l.color)).addTo(map);
  } else {
    const g = layers[top.idx];
    const h3 = latLngToCell(e.lngLat.lat, e.lngLat.lng, gridRes);
    const asInt = BigInt('0x' + h3).toString();
    popup.setLngLat(e.lngLat).setHTML(
      `<div class="pv"><div class="pv-title"><span class="pv-dot" style="background:${g.color}"></span>Ячейка H3</div>` +
      `<table>` +
      `<tr><td class="pv-key">hex</td><td class="pv-val"><span class="pv-json">${h3}</span></td></tr>` +
      `<tr><td class="pv-key">int64</td><td class="pv-val"><span class="pv-num">${asInt}</span></td></tr>` +
      `<tr><td class="pv-key">разрешение</td><td class="pv-val"><span class="pv-num">${gridRes}</span></td></tr>` +
      `</table></div>`
    ).addTo(map);
  }
});

map.on('mousemove', e => {
  const top = topObjectAt(e.point);
  if (top && top.kind === 'table') {
    // подсвечиваем полигон, гасим гексагон
    const l = layers[top.idx];
    const fid = top.feature.id;
    if (!hovered || hovered.srcId !== l.id || hovered.fid !== fid) {
      clearPolyHover();
      hovered = { srcId: l.id, fid };
      map.setFeatureState({ source: l.id, id: fid }, { hover: true });
    }
    if (gridLayerObj) gridLayerObj.setHighlight(null);
    map.getCanvas().style.cursor = 'pointer';
  } else if (top && top.kind === 'grid') {
    clearPolyHover();
    gridLayerObj.setHighlight(latLngToCell(e.lngLat.lat, e.lngLat.lng, gridRes));
    map.getCanvas().style.cursor = '';
  } else {
    clearPolyHover();
    if (gridLayerObj) gridLayerObj.setHighlight(null);
    map.getCanvas().style.cursor = '';
  }
});

map.getCanvas().addEventListener('mouseleave', () => {
  clearPolyHover();
  if (gridLayerObj) gridLayerObj.setHighlight(null);
});

// ---------- панель слоёв ----------
function renderLayersUI() {
  const box = $('layers');
  box.innerHTML = '';
  layers.forEach((l, i) => {
    const row = document.createElement('div');
    row.className = 'lrow';
    if (l.kind === 'table') {
      row.innerHTML =
        `<div class="drag" draggable="true" title="перетащить">⣿</div>` +
        `<input type="checkbox" class="lvis" ${l.visible ? 'checked' : ''} title="показывать слой">` +
        `<input type="color" value="${l.color}" title="цвет слоя">` +
        `<input type="text" placeholder="база.таблица" value="${esc(l.table)}">` +
        `<input type="text" placeholder="поле полигонов" value="${esc(l.geo)}">` +
        `<div class="ops">` +
          `<button type="button" title="выше">↑</button>` +
          `<button type="button" title="ниже">↓</button>` +
          `<button type="button" title="удалить слой">✕</button>` +
        `</div>`;
      const vis = row.querySelector('.lvis');
      const color = row.querySelector('input[type=color]');
      const [table, geo] = row.querySelectorAll('input[type=text]');
      const [up, down, del] = row.querySelectorAll('.ops button');
      vis.addEventListener('change', () => {
        l.visible = vis.checked;
        const v = l.visible ? 'visible' : 'none';
        for (const suf of ['-fill', '-line', '-sel']) {
          if (map.getLayer(l.id + suf)) map.setLayoutProperty(l.id + suf, 'visibility', v);
        }
        if (l.visible) loadVisible();   // включили — догрузить пропущенное
      });
      color.addEventListener('input', () => { l.color = color.value; restyleLayer(l); });
      table.addEventListener('change', () => { l.table = table.value.trim(); });
      geo.addEventListener('change', () => { l.geo = geo.value.trim(); });
      up.addEventListener('click', () => { if (i > 0) move(i, i - 1); });
      down.addEventListener('click', () => { if (i < layers.length - 1) move(i, i + 1); });
      del.addEventListener('click', () => {
        layers.splice(i, 1); renderLayersUI(); rebuildMapLayers(); updateCount(); markDirty();
      });
    } else {
      row.classList.add('gridrow');
      row.innerHTML =
        `<div class="drag" draggable="true" title="перетащить">⣿</div>` +
        `<input type="color" value="${l.color}" title="цвет сетки">` +
        `<label class="gridlabel"><input type="checkbox" ${l.enabled ? 'checked' : ''}> Сетка H3</label>` +
        `<select title="разрешение H3">` +
          `<option value="auto"${l.resolution === 'auto' ? ' selected' : ''}>авто</option>` +
          [5, 6, 7, 8, 9, 10, 11].map(r =>
            `<option value="${r}"${String(l.resolution) === String(r) ? ' selected' : ''}>${r}</option>`).join('') +
        `</select>` +
        `<input type="number" class="gwidth" value="${l.width || 2}" min="1" max="10" step="0.5" title="толщина линий, px">` +
        `<div class="ops">` +
          `<button type="button" title="выше">↑</button>` +
          `<button type="button" title="ниже">↓</button>` +
        `</div>`;
      const color = row.querySelector('input[type=color]');
      const chk = row.querySelector('input[type=checkbox]');
      const sel = row.querySelector('select');
      const gw = row.querySelector('.gwidth');
      const [up, down] = row.querySelectorAll('.ops button');
      color.addEventListener('input', () => { l.color = color.value; restyleLayer(l); });
      chk.addEventListener('change', () => { l.enabled = chk.checked; rebuildMapLayers(); });
      sel.addEventListener('change', () => { l.resolution = sel.value === 'auto' ? 'auto' : +sel.value; rebuildGrid(); });
      gw.addEventListener('change', () => { l.width = +gw.value || 2; rebuildMapLayers(); });
      up.addEventListener('click', () => { if (i > 0) move(i, i - 1); });
      down.addEventListener('click', () => { if (i < layers.length - 1) move(i, i + 1); });
    }
    // перетаскивание мышкой
    const handle = row.querySelector('.drag');
    handle.addEventListener('dragstart', e => {
      e.dataTransfer.setData('text/plain', String(i));
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setDragImage(row, 10, 10);
      row.classList.add('dragging');
    });
    handle.addEventListener('dragend', () => row.classList.remove('dragging'));
    row.addEventListener('dragover', e => { e.preventDefault(); row.classList.add('dragover'); });
    row.addEventListener('dragleave', () => row.classList.remove('dragover'));
    row.addEventListener('drop', e => {
      e.preventDefault();
      row.classList.remove('dragover');
      const from = +e.dataTransfer.getData('text/plain');
      if (!Number.isNaN(from) && from !== i) move(from, i);
    });
    box.appendChild(row);
  });
  updateGridResOptions();
}

function move(from, to) {
  const [x] = layers.splice(from, 1);
  layers.splice(to, 0, x);
  renderLayersUI(); rebuildMapLayers(); markDirty();
}

// ---------- вход по токену ----------
// Пароль ClickHouse браузер больше не видит: сервер берёт его из PAM.
// От пользователя нужен только токен доступа, и тот один раз — дальше cookie.
let serverInfo = { auth: false, logged_in: true };

async function loadInfo() {
  serverInfo = await (await fetch('/api/info')).json();
  $('srvUrl').textContent = serverInfo.clickhouse || '—';
  $('srvCreds').textContent = serverInfo.credentials || '—';
  fillBasemaps(serverInfo.basemaps);
  const hint = document.querySelector('.lhint');
  if (hint && serverInfo.version) hint.textContent += ` · v${serverInfo.version}`;
  return serverInfo;
}

function fillBasemaps(list) {
  basemaps = (list && list.length) ? list : [FALLBACK_BASEMAP];
  const sel = $('basemap');
  sel.innerHTML = '';
  basemaps.forEach((b, i) => {
    const o = document.createElement('option');
    o.value = String(i);
    o.textContent = b.name;
    sel.appendChild(o);
  });
  // выбор подложки личный, поэтому помним его в браузере, а не на сервере
  let saved = 0;
  try { saved = Math.max(0, basemaps.findIndex(b => b.name === localStorage.getItem('basemap'))); } catch {}
  sel.value = String(saved);
  setBasemap(saved, true);
}

function setBasemap(i, initial) {
  const b = basemaps[i];
  if (!b) return;
  basemapIndex = i;
  try { localStorage.setItem('basemap', b.name); } catch {}

  if (initial && map.getSource('base')) {
    // стиль уже такой же — незачем пересобирать слои
    const cur = map.getStyle().sources.base;
    if (cur && String(cur.tiles) === String(b.tiles)) return;
  }
  // setStyle убирает все слои, включая наши: ставим их заново, когда
  // новый стиль загрузится
  map.setStyle(basemapStyle(b));
  map.once('styledata', () => {
    rebuildMapLayers();
    for (const l of layers) {
      if (l.kind === 'table' && map.getSource(l.id)) {
        map.getSource(l.id).setData({ type: 'FeatureCollection', features: l.features });
      }
    }
  });
}

$('basemap').addEventListener('change', e => setBasemap(+e.target.value));

function showLogin(show) { $('login').style.display = show ? 'flex' : 'none'; }

$('loginForm').addEventListener('submit', async e => {
  e.preventDefault();
  $('loginErr').textContent = '';
  try {
    const r = await fetch('/api/login', {
      method: 'POST', headers: JSON_HDR,
      body: JSON.stringify({ token: $('loginToken').value })
    });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || 'не удалось войти');
    showLogin(false);
    $('loginToken').value = '';
    await start();
  } catch (err) { $('loginErr').textContent = err.message; }
});

// ---------- общие шаблоны слоёв ----------
// Раньше настройки лежали у каждого в config.json рядом с exe. Теперь они
// хранятся на сервере и видны всем: один собрал набор слоёв — остальные
// загружают его в один клик.

function currentLayers() {
  return layers.map(l => l.kind === 'table'
    ? { kind: 'table', table: l.table, geo: l.geo, color: l.color, visible: l.visible }
    : { kind: 'grid', color: l.color, resolution: l.resolution, enabled: l.enabled, width: l.width });
}

function applyLayers(list) {
  if (Array.isArray(list) && list.length) {
    // читаем терпимо: битые элементы пропускаем, отсутствующие поля — по умолчанию
    layers = list.filter(l => l && typeof l === 'object').map(l => l.kind === 'grid'
      ? newGridLayer(l.color, l.resolution, l.enabled, l.width)
      : newTableLayer(l.table, l.geo, l.color, l.visible));
  } else {
    layers = [newTableLayer('', 'key')];
  }
  if (!layers.some(l => l.kind === 'grid')) layers.push(newGridLayer());
  renderLayersUI();
}

async function loadTemplateList() {
  const sel = $('tplList');
  const keep = sel.value;
  try {
    const d = await (await fetch('/api/templates')).json();
    sel.innerHTML = '<option value="">— выберите шаблон —</option>';
    for (const t of (d.templates || [])) {
      const o = document.createElement('option');
      o.value = t.id;
      o.textContent = t.name;
      sel.appendChild(o);
    }
    sel.value = keep;
  } catch (e) { console.error(e); }
}

async function saveTemplate() {
  // Диалоги браузера (prompt/confirm) доступны не во всех окружениях —
  // имя берём из поля панели, а подтверждение делаем повторным нажатием.
  const name = $('tplName').value.trim() || ($('tplList').value ? $('tplList').selectedOptions[0].textContent : '');
  if (!name) { $('tplName').focus(); throw new Error('укажите название шаблона'); }
  const r = await fetch('/api/templates', {
    method: 'POST', headers: JSON_HDR,
    body: JSON.stringify({ name, layers: currentLayers() })
  });
  const d = await r.json();
  if (!r.ok) throw new Error(d.error || 'не сохранилось');
  await loadTemplateList();
  $('tplList').value = d.id;
  $('tplName').value = '';
  return d.name;
}

async function loadTemplate(id) {
  const t = await (await fetch('/api/templates/' + encodeURIComponent(id))).json();
  if (t.error) throw new Error(t.error);
  applyLayers(t.layers);
  connect();
}

async function deleteTemplate(id) {
  const r = await fetch('/api/templates/' + encodeURIComponent(id), { method: 'DELETE', headers: JSON_HDR });
  if (!r.ok) throw new Error((await r.json()).error || 'не удалось удалить');
  await loadTemplateList();
  $('tplList').value = '';
}

async function chQuery(sql) {
  lastSQL = sql;
  $('showSql').style.display = '';
  const resp = await fetch('/api/query', {
    method: 'POST', headers: JSON_HDR,
    body: JSON.stringify({ sql })
  });
  const data = await resp.json();
  if (data.error) throw new Error(data.error);
  return data;
}

async function describeLayer(l) {
  const d = await chQuery(`DESCRIBE TABLE ${l.table} FORMAT JSON`);
  const names = d.data.map(r => r.name);
  if (!names.includes(l.geo)) {
    throw new Error(`В таблице ${l.table} нет колонки «${l.geo}». Есть колонки: ${names.join(', ')}`);
  }
}

async function loadLayer(l, signal) {
  const b = map.getBounds();
  // размер MIN_OBJ_PX пикселей в градусах на текущем масштабе:
  // объекты мельче не грузим — на экране их всё равно не разглядеть
  const minDeg = (b.getEast() - b.getWest()) / map.getContainer().clientWidth * MIN_OBJ_PX;
  const resp = await fetch('/api/objects', {
    method: 'POST', headers: JSON_HDR, signal,
    body: JSON.stringify({
      table: l.table, geo: l.geo,
      bbox: [b.getWest(), b.getSouth(), b.getEast(), b.getNorth()],
      limit: MAX_ROWS, min_deg: minDeg, reset: l.needReset
    })
  });
  const data = await resp.json();
  if (data.error) {
    if (data.sql) lastSQL = data.sql;
    throw new Error(`Слой ${l.table}: ${data.error}`);
  }
  l.needReset = false;
  if (data.sql) { lastSQL = data.sql; $('showSql').style.display = ''; }

  let added = 0;
  for (const row of data.data) {
    const id = String(row.__id);
    if (l.loaded.has(id)) continue;
    l.loaded.add(id);
    // __id остаётся в свойствах для выделения; попап поля с «__» не показывает
    const props = {};
    for (const k of Object.keys(row)) {
      if (k !== l.geo) props[k] = row[k];
    }
    l.features.push({
      type: 'Feature',
      geometry: { type: 'MultiPolygon', coordinates: row[l.geo] },
      properties: props
    });
    added++;
  }
  if (added && mapReady && map.getSource(l.id)) {
    map.getSource(l.id).setData({ type: 'FeatureCollection', features: l.features });
  }
  return data;
}

async function loadVisible() {
  rebuildGrid();
  if (!connected) return;
  const tables = layers.filter(l => l.kind === 'table' && l.table && l.visible);
  if (!tables.length) { updateCount(); return; }
  if (inFlight) inFlight.abort();
  inFlight = new AbortController();
  say('Загрузка…');
  try {
    const results = await Promise.all(tables.map(l => loadLayer(l, inFlight.signal)));
    const cached = results.every(r => r.cached);
    const capped = results.some(r => r.rows >= MAX_ROWS);
    updateCount((cached ? ' (из кэша, без запроса к БД)' : '') +
                (capped ? ' (лимит запроса, приблизьте карту)' : ''));
  } catch (e) {
    if (e.name === 'AbortError') return;
    say(e.message.slice(0, 150), true);
    showSqlBox(e.message);
  }
}

async function connect() {
  const active = layers.filter(l => l.kind === 'table' && l.table);
  if (!active.length) { say('Добавьте хотя бы один слой с таблицей', true); return; }
  for (const l of layers) {
    if (l.kind !== 'table') continue;
    l.loaded.clear();
    l.features = [];
    l.needReset = true;
  }
  say('Читаю структуру таблиц…');
  try {
    for (const l of active) await describeLayer(l);
  } catch (e) {
    connected = false;
    say(e.message.slice(0, 150), true);
    showSqlBox(e.message);
    return;
  }
  connected = true;
  collapse(true);
  rebuildMapLayers();
  loadVisible();
}

map.on('moveend', () => {
  updateGridResOptions();
  clearTimeout(debounceTimer);
  debounceTimer = setTimeout(loadVisible, 300);
});

// ---------- панель ----------
function collapse(yes) {
  $('panel').classList.toggle('collapsed', yes);
  $('showBtn').style.display = yes ? '' : 'none';
}
$('hideBtn').addEventListener('click', () => collapse(true));
$('showBtn').addEventListener('click', () => collapse(false));
$('addLayer').addEventListener('click', () => {
  // новый слой добавляем над сеткой, если она в конце
  layers.push(newTableLayer('', 'key'));
  renderLayersUI(); markDirty();
});
$('f').addEventListener('submit', e => { e.preventDefault(); connect(); });

function markDirty() { $('saveBtn').classList.remove('secondary'); }
function markSaved() { $('saveBtn').classList.add('secondary'); }
$('f').addEventListener('input', e => { if (e.target !== $('saveBtn')) markDirty(); });

$('saveBtn').addEventListener('click', async () => {
  try {
    const name = await saveTemplate();
    if (name) { msg.textContent = 'Шаблон «' + name + '» сохранён'; markSaved(); }
  } catch (e) { msg.textContent = 'Не сохранилось: ' + e.message; }
});
$('tplLoad').addEventListener('click', async () => {
  const id = $('tplList').value;
  if (!id) { msg.textContent = 'Сначала выберите шаблон'; return; }
  try { await loadTemplate(id); msg.textContent = ''; markSaved(); }
  catch (e) { msg.textContent = 'Не загрузилось: ' + e.message; }
});
let delArmed = '';
$('tplDel').addEventListener('click', async () => {
  const id = $('tplList').value;
  if (!id) { msg.textContent = 'Сначала выберите шаблон'; return; }
  if (delArmed !== id) {   // первое нажатие только предупреждает
    delArmed = id;
    msg.textContent = 'Шаблон общий: нажмите «удалить» ещё раз, чтобы удалить его у всех';
    setTimeout(() => { if (delArmed === id) { delArmed = ''; msg.textContent = ''; } }, 5000);
    return;
  }
  delArmed = '';
  try { await deleteTemplate(id); msg.textContent = 'Шаблон удалён'; }
  catch (e) { msg.textContent = 'Не удалилось: ' + e.message; }
});
$('showSql').addEventListener('click', () => showSqlBox());
$('closeSql').addEventListener('click', () => $('sqlbox').classList.remove('show'));
$('copySql').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText(lastSQL); $('copySql').textContent = 'Скопировано!'; }
  catch {
    const r = document.createRange(); r.selectNodeContents($('sqltext'));
    const s = getSelection(); s.removeAllRanges(); s.addRange(r);
    $('copySql').textContent = 'Выделено — Ctrl+C';
  }
  setTimeout(() => $('copySql').textContent = 'Скопировать SQL', 2000);
});

// ---------- контекстный поиск по видимым слоям ----------
let searchTimer = null;
let searchAbort = null;
const sIn = $('searchInput'), sRes = $('searchRes'), sClr = $('searchClear');

function selectObject(l, id) {
  for (const t of layers) {
    if (t.kind !== 'table' || !map.getLayer(t.id + '-sel')) continue;
    map.setFilter(t.id + '-sel', ['==', ['get', '__id'], t === l ? String(id) : ' ']);
  }
}
function clearSelection() { selectObject(null, ' '); }
popup.on('close', clearSelection);

function summaryOf(row) {
  const parts = [];
  for (const [k, v] of Object.entries(row)) {
    if (k.startsWith('__') || v === null || v === '') continue;
    parts.push(String(v));
    if (parts.length >= 4) break;
  }
  return parts.join(' · ');
}

function hideSearch() { sRes.classList.remove('show'); }

async function runSearch() {
  const q = sIn.value.trim();
  if (q.length < 2 || !connected) { hideSearch(); return; }
  if (searchAbort) searchAbort.abort();
  searchAbort = new AbortController();
  const visibleTables = layers.filter(l => l.kind === 'table' && l.table && l.visible);
  if (!visibleTables.length) return;
  try {
    const perLayer = await Promise.all(visibleTables.map(async l => {
      const resp = await fetch('/api/search', {
        method: 'POST', headers: JSON_HDR, signal: searchAbort.signal,
        body: JSON.stringify({ table: l.table, geo: l.geo, query: q, limit: 50 })
      });
      const data = await resp.json();
      if (data.error) throw new Error(`Слой ${l.table}: ${data.error}`);
      return data.data.map(row => ({ l, row }));
    }));
    const results = perLayer.flat();
    sRes.innerHTML = '';
    if (!results.length) {
      sRes.innerHTML = '<div class="sr-none">Ничего не найдено</div>';
    } else {
      for (const { l, row } of results.slice(0, 100)) {
        const item = document.createElement('div');
        item.className = 'sr-item';
        item.innerHTML =
          `<span class="sr-dot" style="background:${l.color}"></span>` +
          `<span class="sr-text">${esc(summaryOf(row))}</span>` +
          `<span class="sr-table">${esc(l.table)}</span>`;
        item.addEventListener('click', () => gotoResult(l, row));
        sRes.appendChild(item);
      }
    }
    sRes.classList.add('show');
  } catch (e) {
    if (e.name === 'AbortError') return;
    sRes.innerHTML = `<div class="sr-none">${esc(e.message.slice(0, 200))}</div>`;
    sRes.classList.add('show');
  }
}

// перелёт к найденному объекту: зум по его bbox, выделение и попап со свойствами
function gotoResult(l, row) {
  hideSearch();
  const w = +row.__w, e = +row.__e, s = +row.__s, n = +row.__n;
  map.fitBounds([[w, s], [e, n]], { padding: 120, maxZoom: 16, duration: 800 });
  selectObject(l, row.__id);
  popup.setLngLat([(w + e) / 2, (s + n) / 2])
       .setHTML(popupHTML(row, l.table, l.color))
       .addTo(map);
}

sIn.addEventListener('input', () => {
  sClr.style.display = sIn.value ? '' : 'none';
  clearTimeout(searchTimer);
  searchTimer = setTimeout(runSearch, 400);
});
sIn.addEventListener('keydown', e => {
  if (e.key === 'Escape') { hideSearch(); sIn.blur(); }
  if (e.key === 'Enter') {
    const first = sRes.querySelector('.sr-item');
    if (first) first.click();
  }
});
sClr.addEventListener('click', () => {
  sIn.value = ''; sClr.style.display = 'none';
  hideSearch(); clearSelection();
});
document.addEventListener('click', e => {
  if (!$('search').contains(e.target)) hideSearch();
});

// ---------- старт ----------
// Сначала спрашиваем сервер, нужен ли вход. Если нужен и cookie ещё нет —
// показываем экран входа; всё остальное загружается уже после него.
async function start() {
  await loadTemplateList();
  applyLayers(null);
  say('Выберите шаблон или задайте слои и нажмите «Показать на карте»');
}

(async () => {
  try {
    const info = await loadInfo();
    if (info.auth && !info.logged_in) { showLogin(true); return; }
    await start();
  } catch (e) {
    say('Сервер недоступен: ' + e.message, true);
  }
})();
