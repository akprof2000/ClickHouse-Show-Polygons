// Перепроецирование растровых тайлов из EPSG:3395 в EPSG:3857.
//
// Яндекс отдаёт тайлы в эллипсоидальном меркаторе (EPSG:3395), а карта и
// данные у нас в сферическом (EPSG:3857). Если подставить такие тайлы как
// есть, подложка съезжает на север: в Москве примерно на двадцать километров.
//
// Исправить это можно дёшево, потому что у двух проекций одинаковая долгота:
// различается только широта, то есть вертикальная ось. Значит, каждый тайл
// 3857 собирается построчно — для каждой строки пикселей считаем её широту,
// находим, на какую строку какого тайла 3395 она приходится, и копируем эту
// строку. Нужны при этом один-два исходных тайла, не больше.
//
// Работает целиком в браузере через свой протокол maplibre: тайлы по-прежнему
// идут от поставщика напрямую, сервер в этом не участвует.

const E = 0.0818191908426215; // эксцентриситет эллипсоида WGS84
const TILE = 256;

// широта (в радианах) глобальной пиксельной строки сферического меркатора
function latFromY3857(y, size) {
  return Math.atan(Math.sinh(Math.PI * (1 - 2 * y / size)));
}

// глобальная пиксельная строка той же широты в эллипсоидальном меркаторе
function y3395FromLat(phi, size) {
  const s = Math.sin(phi);
  const t = Math.tan(Math.PI / 4 + phi / 2) * Math.pow((1 - E * s) / (1 + E * s), E / 2);
  return size / 2 * (1 - Math.log(t) / Math.PI);
}

function fillTemplate(tpl, z, x, y) {
  return tpl.replace('{z}', z).replace('{x}', x).replace('{y}', y);
}

// Небольшой кэш уже разобранных исходных тайлов: соседние тайлы 3857 берут
// строки из одних и тех же тайлов 3395, и декодировать их повторно незачем.
const CACHE_LIMIT = 256;
const cache = new Map();

function sourceTile(url, signal) {
  let p = cache.get(url);
  if (p) {
    cache.delete(url); // освежаем порядок: недавно нужные живут дольше
    cache.set(url, p);
    return p;
  }
  p = fetch(url, { signal, mode: 'cors' })
    .then(r => {
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      return r.blob();
    })
    .then(b => createImageBitmap(b));
  p.catch(() => cache.delete(url)); // неудачу не кэшируем
  cache.set(url, p);
  if (cache.size > CACHE_LIMIT) {
    const oldest = cache.keys().next().value;
    cache.get(oldest).then(bmp => bmp.close?.(), () => {});
    cache.delete(oldest);
  }
  return p;
}

const templates = new Map();
let registered = false;

// Регистрирует протокол один раз и возвращает адрес тайлов, который нужно
// подставить в источник вместо исходного шаблона.
export function reprojectedTiles(maplibregl, template) {
  if (!registered) {
    maplibregl.addProtocol('merc3395', loadTile);
    registered = true;
  }
  let id = [...templates.entries()].find(([, t]) => t === template)?.[0];
  if (id === undefined) {
    id = templates.size;
    templates.set(id, template);
  }
  return `merc3395://${id}/{z}/{x}/{y}`;
}

async function loadTile(params, abortController) {
  const m = /^merc3395:\/\/(\d+)\/(\d+)\/(\d+)\/(\d+)/.exec(params.url);
  if (!m) throw new Error('некорректный адрес тайла: ' + params.url);
  const [id, z, x, y] = m.slice(1).map(Number);
  const tpl = templates.get(id);
  const n = 2 ** z;
  const size = TILE * n;

  // для каждой строки итогового тайла: какой исходный тайл и какая в нём строка
  const rows = new Array(TILE);
  const need = new Set();
  for (let py = 0; py < TILE; py++) {
    const ys = y3395FromLat(latFromY3857(y * TILE + py + 0.5, size), size);
    const ty = Math.floor(ys / TILE);
    if (ty < 0 || ty >= n) { rows[py] = null; continue; }
    rows[py] = { ty, row: Math.min(TILE - 1, Math.floor(ys - ty * TILE)) };
    need.add(ty);
  }

  const bitmaps = new Map();
  await Promise.all([...need].map(async ty => {
    try {
      bitmaps.set(ty, await sourceTile(fillTemplate(tpl, z, x, ty), abortController.signal));
    } catch (e) {
      if (abortController.signal.aborted) throw e;
      // один недоступный исходный тайл не должен ронять весь: строки останутся пустыми
    }
  }));
  if (bitmaps.size === 0) throw new Error('исходные тайлы недоступны');

  const canvas = new OffscreenCanvas(TILE, TILE);
  const ctx = canvas.getContext('2d');
  for (let py = 0; py < TILE; py++) {
    const r = rows[py];
    const bmp = r && bitmaps.get(r.ty);
    if (bmp) ctx.drawImage(bmp, 0, r.row, TILE, 1, 0, py, TILE, 1);
  }
  const blob = await canvas.convertToBlob({ type: 'image/png' });
  return { data: await blob.arrayBuffer() };
}

// Для проверки: смещение на широте lat (в градусах) в пикселях зума z.
export function shiftPixels(latDeg, z) {
  const size = TILE * 2 ** z;
  const phi = latDeg * Math.PI / 180;
  const y3857 = size / 2 * (1 - Math.log(Math.tan(Math.PI / 4 + phi / 2)) / Math.PI);
  return y3395FromLat(phi, size) - y3857;
}
