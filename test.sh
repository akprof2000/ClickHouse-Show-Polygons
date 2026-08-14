#!/bin/bash
# Автотест chviewer: сборка, запуск, проверка запросов к ClickHouse по http и https.
# Требует: запущенный ClickHouse в docker (ch-test) на 8123 (http) и 8443 (https, self-signed),
# пользователь default / test123, таблица emr_ch.tbl_polygons_bmt с 200 строками.
set -u
cd "$(dirname "$0")"
PASS=0; FAIL=0
APP=http://127.0.0.1:8137

ok()   { PASS=$((PASS+1)); echo "  [OK]   $1"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $1"; }
check() { # check "описание" условие(0=ок)
  if [ "$2" -eq 0 ]; then ok "$1"; else fail "$1"; fi
}

query() { # query <ch_url> <insecure> [password]
  local pw="${3-test123}"
  curl -s -X POST "$APP/api/query" --data-binary "{
    \"url\":\"$1\",\"user\":\"default\",\"password\":\"$pw\",
    \"insecure\":$2,
    \"sql\":\"SELECT * FROM emr_ch.tbl_polygons_bmt LIMIT 190 FORMAT JSON\"}"
}

echo "=== 1. Предусловия: ClickHouse доступен ==="
curl -s --max-time 5 http://localhost:8123/ping | grep -q Ok; check "http  8123 ping" $?
curl -sk --max-time 5 https://localhost:8443/ping | grep -q Ok; check "https 8443 ping" $?

echo "=== 2. Сборка ==="
VER=$(tr -d ' \r\n' < VERSION)
go build -ldflags "-s -w -X main.version=$VER" -o chviewer.exe .; check "go build" $?

echo "=== 3. Запуск приложения ==="
taskkill //F //IM chviewer.exe >/dev/null 2>&1; sleep 1
[ -f config.json ] && cp config.json config.json.bak
./chviewer.exe -nobrowser & APP_PID=$!
for i in $(seq 1 20); do curl -s --max-time 2 "$APP/" >/dev/null 2>&1 && break; sleep 0.5; done
curl -s "$APP/" | grep -q "ClickHouse Viewer"; check "страница отдаётся" $?

echo "=== 4. Конфиг: сохранение и чтение ==="
curl -s -X POST "$APP/api/config" --data-binary '{"url":"localhost","user":"default","pass_enc":"x.y.z","table":"emr_ch.tbl_polygons_bmt","geo":"key","limit":190,"ssl":true,"insecure":true}' | grep -q '"ok":true'
check "POST /api/config" $?
curl -s "$APP/api/config" | grep -q '"pass_enc":"x.y.z"'; check "GET /api/config возвращает сохранённое" $?
[ -f config.json ]; check "config.json создан рядом с exe" $?
grep -q test123 config.json 2>/dev/null; check "пароля в открытом виде нет в config.json" $((! $?))

echo "=== 5. Запросы без SSL (http://localhost:8123) ==="
R=$(query "http://localhost:8123/" false)
echo "$R" | grep -q '"rows": 190'; check "получено 190 строк" $?
echo "$R" | grep -q 'БМТ участок'; check "кириллица не побилась" $?
echo "$R" | tr -d ' \n\t' | grep -q '"key":\[\[\[\[37\.5'; check "мультиполигон пришёл вложенными массивами" $?

echo "=== 6. Запросы по SSL (https://localhost:8443) ==="
R=$(query "https://localhost:8443/" true)
echo "$R" | grep -q '"rows": 190'; check "self-signed + insecure=true: 190 строк" $?
R=$(query "https://localhost:8443/" false)
echo "$R" | grep -qi 'error.*certificate\|certificate.*error\|x509'; check "insecure=false: осмысленная ошибка сертификата" $?

echo "=== 6б. Bbox-фильтр (читаем только видимую область) ==="
BBOX_SQL='WITH flatten(flatten(key)) AS pts, arrayMap(p -> p.1, pts) AS xs, arrayMap(p -> p.2, pts) AS ys SELECT bmt FROM emr_ch.tbl_polygons_bmt WHERE arrayMin(xs) <= 37.6 AND arrayMax(xs) >= 37.4 AND arrayMin(ys) <= 55.9 AND arrayMax(ys) >= 55.6 FORMAT JSON'
R=$(curl -s -X POST "$APP/api/query" --data-binary "{\"url\":\"http://localhost:8123/\",\"user\":\"default\",\"password\":\"test123\",\"insecure\":false,\"sql\":\"$BBOX_SQL\"}")
ROWS=$(echo "$R" | grep -o '"rows": [0-9]*' | grep -o '[0-9]*')
[ -n "$ROWS" ] && [ "$ROWS" -gt 0 ] && [ "$ROWS" -lt 200 ]; check "bbox вернул часть данных ($ROWS из 200)" $?
BBOX_SQL2='WITH flatten(flatten(key)) AS pts, arrayMap(p -> p.1, pts) AS xs, arrayMap(p -> p.2, pts) AS ys SELECT bmt FROM emr_ch.tbl_polygons_bmt WHERE arrayMin(xs) <= 30.0 AND arrayMax(xs) >= 29.0 AND arrayMin(ys) <= 50.0 AND arrayMax(ys) >= 49.0 FORMAT JSON'
R=$(curl -s -X POST "$APP/api/query" --data-binary "{\"url\":\"http://localhost:8123/\",\"user\":\"default\",\"password\":\"test123\",\"insecure\":false,\"sql\":\"$BBOX_SQL2\"}")
echo "$R" | grep -q '"rows": 0'; check "bbox вне данных вернул 0 строк" $?

echo "=== 6в. Кэш объектов на бэке (/api/objects) ==="
OBJ_BODY='{"url":"http://localhost:8123/","user":"default","password":"test123","insecure":false,"table":"emr_ch.tbl_polygons_bmt","geo":"key","bbox":[37.4,55.6,38.0,56.0],"limit":3000,"reset":RESET}'
R=$(curl -s -X POST "$APP/api/objects" --data-binary "${OBJ_BODY/RESET/true}")
echo "$R" | grep -q '"cached":false'; check "первый запрос идёт в БД" $?
N1=$(echo "$R" | grep -o '"total_cached":[0-9]*' | grep -o '[0-9]*')
[ "$N1" = "200" ]; check "в кэш попали все 200 объектов" $?
R=$(curl -s -X POST "$APP/api/objects" --data-binary "${OBJ_BODY/RESET/false}")
echo "$R" | grep -q '"cached":true'; check "повторный запрос отдан из кэша без БД" $?
R=$(curl -s -X POST "$APP/api/objects" --data-binary '{"url":"http://localhost:8123/","user":"default","password":"test123","insecure":false,"table":"emr_ch.tbl_polygons_bmt","geo":"key","bbox":[37.45,55.65,37.55,55.75],"limit":3000,"reset":false}')
NROWS=$(echo "$R" | grep -o '"rows":[0-9]*' | grep -o '[0-9]*')
echo "$R" | grep -q '"cached":true' && [ -n "$NROWS" ] && [ "$NROWS" -gt 0 ] && [ "$NROWS" -lt 200 ]
check "малый bbox внутри покрытой области: из кэша, часть объектов ($NROWS)" $?

echo "=== 6г. Контекстный поиск (/api/search) ==="
# кириллицу передаём через файл: аргументы командной строки Windows перекодирует
printf '%s' '{"url":"http://localhost:8123/","user":"default","password":"test123","insecure":false,"table":"emr_ch.tbl_polygons_bmt","geo":"key","query":"участок 96","limit":50}' > /tmp/search_q.json
R=$(curl -s -X POST "$APP/api/search" --data-binary @/tmp/search_q.json)
echo "$R" | grep -q 'участок 96'; check "поиск по строке нашёл объект" $?
echo "$R" | grep -q '"__w"'; check "результат содержит bbox для перелёта" $?
R=$(curl -s -X POST "$APP/api/search" --data-binary '{"url":"http://localhost:8123/","user":"default","password":"test123","insecure":false,"table":"emr_ch.tbl_polygons_bmt","geo":"key","query":"1095","limit":50}')
echo "$R" | grep -q '"rows":[1-9]'; check "поиск по числовому полю работает" $?
R=$(curl -s -X POST "$APP/api/search" --data-binary '{"url":"http://localhost:8123/","user":"default","password":"test123","insecure":false,"table":"emr_ch.tbl_polygons_bmt","geo":"key","query":"такого-нет-нигде","limit":50}')
echo "$R" | grep -q '"rows":0'; check "несуществующая строка -> 0 результатов" $?

echo "=== 7. Ошибочные сценарии ==="
R=$(query "http://localhost:8123/" false "wrongpass")
echo "$R" | grep -qi 'error'; check "неверный пароль -> ошибка" $?
R=$(query "http://localhost:9999/" false)
echo "$R" | grep -qi 'error'; check "недоступный порт -> ошибка" $?

kill $APP_PID 2>/dev/null
taskkill //F //IM chviewer.exe >/dev/null 2>&1
[ -f config.json.bak ] && mv -f config.json.bak config.json || rm -f config.json

echo
echo "================================"
echo "Пройдено: $PASS, провалено: $FAIL"
[ $FAIL -eq 0 ]
