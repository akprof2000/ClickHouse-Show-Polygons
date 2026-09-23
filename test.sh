#!/bin/bash
# Автотест chviewer в серверном режиме.
# Требует: запущенный ClickHouse в docker (ch-test) на 9000 (native) и 9440
# (native + TLS, self-signed), пользователь default / test123, таблица
# emr_ch.tbl_polygons_bmt с 200 строками (см. testenv/setup.sql).
set -u
cd "$(dirname "$0")"
PASS=0; FAIL=0
PORT=8137                       # для тестов берём свободный порт, не рабочий 8081
APP="http://127.0.0.1:$PORT"
TOKEN="test-token-$$"
CT="Content-Type: application/json"
COOKIE=""                       # заполняется после входа

# Тест гоняется и на Windows (git-bash), и в GitHub Actions на Linux
if [ "${OS:-}" = "Windows_NT" ]; then BIN=./chviewer-test.exe; else BIN=./chviewer-test; fi
APP_PID=""
stop_server() {
  [ -n "$APP_PID" ] && kill "$APP_PID" 2>/dev/null
  # на Windows kill по PID из MSYS не всегда достаёт до процесса Windows
  command -v taskkill >/dev/null 2>&1 && taskkill //F //IM "$(basename "$BIN")" >/dev/null 2>&1
  APP_PID=""
  return 0
}

ok()   { PASS=$((PASS+1)); echo "  [OK]   $1"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $1"; }
check() { if [ "$2" -eq 0 ]; then ok "$1"; else fail "$1"; fi; }

# api <метод> <путь> [тело] — запрос с токеном
api() {
  if [ $# -ge 3 ]; then
    curl -s -X "$1" -H "$CT" -H "Cookie: $COOKIE" --data-binary "$3" "$APP$2"
  else
    curl -s -X "$1" -H "$CT" -H "Cookie: $COOKIE" "$APP$2"
  fi
}
code() { # код ответа без токена
  curl -s -o /dev/null -w '%{http_code}' -X "$1" -H "$CT" --data-binary "${3-{\}}" "$APP$2"
}

echo "=== 1. Предусловия: ClickHouse доступен ==="
curl -s --max-time 5 http://localhost:8123/ping | grep -q Ok; check "http 8123 (для заливки данных)" $?
docker exec ch-test clickhouse-client --password test123 -q "SELECT 1" >/dev/null 2>&1
check "native 9000 отвечает" $?
echo | openssl s_client -connect localhost:9440 2>/dev/null | grep -q CONNECTED
check "native+TLS 9440 слушает" $?

echo "=== 2. Сборка ==="
VER=$(tr -d ' \r\n' < VERSION)
go build -ldflags "-s -w -X main.version=$VER" -o "$BIN" .; check "go build" $?

echo "=== 3. Конфигурация и запуск ==="
TMP=$(mktemp -d)
trap 'stop_server; rm -rf "$TMP"' EXIT
# Go нужен windows-вид пути: для неё /tmp/x это C:\tmp\x,
# а у MSYS тот же /tmp живёт в другом месте
TMPW=$(cygpath -m "$TMP" 2>/dev/null || echo "$TMP")
cp testenv/server.crt "$TMP/server.crt"
cat > "$TMP/chviewer.yaml" <<YAML
listen: "127.0.0.1:$PORT"
title: "chviewer test"
data_dir: "$TMPW/data"
auth:
  token_env: "CHVIEWER_TEST_TOKEN"
clickhouse:
  addr: ["localhost:9000"]
  database: "default"
  tls: "off"
  user: "default"
  password_env: "CH_TEST_PASSWORD"
basemaps:
  - name: "Внутренний"
    tiles: ["http://tiles.corp.local:8080/{z}/{x}/{y}.png"]
  - name: "OpenStreetMap"
    tiles: ["https://tile.openstreetmap.org/{z}/{x}/{y}.png"]
YAML
CH_TEST_PASSWORD=test123 "$BIN" -config "$TMP/chviewer.yaml" -check >/dev/null 2>&1
check "-check проходит с верным паролем" $?

CH_TEST_PASSWORD=wrong "$BIN" -config "$TMP/chviewer.yaml" -check >/dev/null 2>&1
check "-check падает с неверным паролем" $((! $?))

stop_server; sleep 1
CHVIEWER_TEST_TOKEN="$TOKEN" CH_TEST_PASSWORD=test123 \
  "$BIN" -config "$TMP/chviewer.yaml" >"$TMP/server.log" 2>&1 &
APP_PID=$!
for i in $(seq 1 20); do curl -s --max-time 2 "$APP/api/info" >/dev/null 2>&1 && break; sleep 0.5; done
curl -s "$APP/" | grep -q "ClickHouse Viewer"; check "страница отдаётся" $?
curl -s "$APP/api/info" | grep -q '"auth":true'; check "/api/info сообщает, что нужен вход" $?

echo "=== 4. Вход по токену ==="
[ "$(code POST /api/objects)" = "401" ]; check "без токена API отвечает 401" $?
curl -s -o /dev/null -w '%{http_code}' -X POST -H "$CT" --data-binary '{"token":"мимо"}' "$APP/api/login" | grep -q 401
check "неверный токен отклонён" $?
COOKIE=$(curl -s -i -X POST -H "$CT" --data-binary "{\"token\":\"$TOKEN\"}" "$APP/api/login" \
         | grep -i '^set-cookie:' | sed 's/^[Ss]et-[Cc]ookie: *//; s/;.*//')
[ -n "$COOKIE" ]; check "верный токен выдал cookie" $?
api GET /api/info | grep -q '"logged_in":true'; check "после входа сессия признаётся" $?

echo "=== 5. Учётные данные не покидают сервер ==="
curl -s "$APP/" "$APP/bundle.js" | grep -qi 'test123'; check "пароля нет в отданных браузеру файлах" $((! $?))
api GET /api/info | grep -qi 'test123'; check "пароля нет в /api/info" $((! $?))

echo "=== 6. Запросы к ClickHouse (адрес берётся из конфига) ==="
R=$(api POST /api/query '{"sql":"SELECT * FROM emr_ch.tbl_polygons_bmt LIMIT 190"}')
echo "$R" | grep -q '"rows":190'; check "получено 190 строк" $?
echo "$R" | grep -q 'БМТ участок'; check "кириллица не побилась" $?
echo "$R" | tr -d ' \n\t' | grep -q '"key":\[\[\[\[37\.5'; check "мультиполигон пришёл вложенными массивами" $?

echo "=== 7. Кэш объектов ==="
OBJ='{"table":"emr_ch.tbl_polygons_bmt","geo":"key","bbox":[37.4,55.6,38.0,56.0],"limit":3000,"reset":RESET}'
R=$(api POST /api/objects "${OBJ/RESET/true}")
echo "$R" | grep -q '"cached":false'; check "первый запрос идёт в БД" $?
echo "$R" | grep -o '"total_cached":200' | grep -q 200; check "в кэш попали все 200 объектов" $?
R=$(api POST /api/objects "${OBJ/RESET/false}")
echo "$R" | grep -q '"cached":true'; check "повторный запрос отдан из кэша без БД" $?

echo "=== 8. Контекстный поиск ==="
printf '%s' '{"table":"emr_ch.tbl_polygons_bmt","geo":"key","query":"участок 96","limit":50}' > "$TMP/q.json"
R=$(curl -s -X POST -H "$CT" -H "Cookie: $COOKIE" --data-binary @"$TMP/q.json" "$APP/api/search")
echo "$R" | grep -q 'участок 96'; check "поиск по строке нашёл объект" $?
echo "$R" | grep -q '"__w"'; check "результат содержит bbox для перелёта" $?
R=$(api POST /api/search '{"table":"emr_ch.tbl_polygons_bmt","geo":"key","query":"1095","limit":50}')
echo "$R" | grep -q '"rows":[1-9]'; check "поиск по числовому полю работает" $?

echo "=== 9. Общие шаблоны ==="
# Имя латиницей: идентификатор шаблона выводится из имени и попадает в путь
# URL, а кириллицу в пути пришлось бы кодировать вручную. Отдельной проверкой
# ниже убеждаемся, что кириллица в имени тоже переживает сохранение.
T='{"name":"test-template","layers":[{"kind":"table","table":"emr_ch.tbl_polygons_bmt","geo":"key","color":"#1565c0","visible":true}]}'
R=$(api POST /api/templates "$T")
echo "$R" | grep -q '"id":"test-template"'; check "шаблон сохранён" $?
api GET /api/templates | grep -q 'test-template'; check "шаблон виден в общем списке" $?
api GET /api/templates/test-template | grep -q 'tbl_polygons_bmt'; check "шаблон читается по идентификатору" $?
[ -f "$TMPW/data/templates/test-template.json" ]; check "шаблон лежит на сервере, а не у пользователя" $?
api POST /api/templates "$T" >/dev/null
[ "$(api GET /api/templates | grep -o '"id":"test-template"' | wc -l)" = "1" ]
check "повторное сохранение обновляет, а не плодит копии" $?

# кириллицу передаём через файл: аргументы командной строки Windows перекодирует
printf '%s' '{"name":"Участки БМТ","layers":[{"kind":"table","table":"t","geo":"g"}]}' > "$TMP/tpl.json"
curl -s -X POST -H "$CT" -H "Cookie: $COOKIE" --data-binary @"$TMP/tpl.json" "$APP/api/templates" >/dev/null
api GET /api/templates | grep -q 'Участки БМТ'; check "кириллица в названии шаблона сохраняется" $?

api DELETE /api/templates/test-template | grep -q '"ok":true'; check "шаблон удаляется" $?
api GET /api/templates | grep -q 'test-template'; check "после удаления шаблона в списке нет" $((! $?))

echo "=== 10. Режимы TLS до ClickHouse ==="
# конфигурация для проверки режима: chmode <tls> [ca_cert]
chmode() {
  cat > "$TMP/tls.yaml" <<YAML
listen: "127.0.0.1:$((PORT+1))"
data_dir: "$TMPW/data"
auth:
  token_env: "CHVIEWER_TEST_TOKEN"
clickhouse:
  addr: ["localhost:9440"]
  tls: "$1"
  ca_cert: [${2:-}]
  user: "default"
  password_env: "CH_TEST_PASSWORD"
YAML
  CH_TEST_PASSWORD=test123 "$BIN" -config "$TMP/tls.yaml" -check >/dev/null 2>&1
}
chmode insecure; check "tls: insecure — соединение с самоподписанным сертификатом" $?
chmode ca "\"$TMPW/server.crt\""; check "tls: ca — сертификат принят из ca_cert" $?
chmode on; check "tls: on — чужой сертификат отвергнут системными корнями" $((! $?))
chmode ca; check "tls: ca без ca_cert — понятная ошибка" $((! $?))
chmode нечто; check "неизвестный режим tls отклонён" $((! $?))

echo "=== 11. Защита от межсайтовых запросов ==="
curl -s -o /dev/null -w '%{http_code}' -X POST -H "$CT" -H "Cookie: $COOKIE" \
  -H "Origin: http://evil.example.com" --data-binary '{}' "$APP/api/objects" | grep -q 403
check "запрос с чужим Origin отклонён" $?
curl -s -o /dev/null -w '%{http_code}' -X POST -H "Content-Type: application/x-www-form-urlencoded" \
  -H "Cookie: $COOKIE" --data-binary '{}' "$APP/api/objects" | grep -q 415
check "запрос без JSON-заголовка отклонён" $?

echo "=== 12. Заголовки страницы ==="
H=$(curl -s -D - -o /dev/null "$APP/")
# OpenStreetMap и другие тайл-серверы требуют Referer; без него на проде 403
echo "$H" | grep -qi '^referrer-policy: strict-origin-when-cross-origin'
check "Referrer-Policy отдаёт тайл-серверам адрес сайта" $?
echo "$H" | grep -qi '^referrer-policy: no-referrer'
check "Referrer-Policy не no-referrer" $((! $?))
# внутренний тайл-сервер по http должен быть разрешён политикой, иначе
# браузер молча не загрузит подложку
echo "$H" | grep -i '^content-security-policy:' | grep -q 'img-src[^;]*http://tiles.corp.local:8080'
check "CSP пропускает подложку по http из конфигурации (картинки)" $?
echo "$H" | grep -i '^content-security-policy:' | grep -q 'connect-src[^;]*http://tiles.corp.local:8080'
check "CSP пропускает подложку по http из конфигурации (запросы)" $?
api GET /api/info | grep -q 'tiles.corp.local'; check "список подложек отдаётся странице" $?

echo "=== 13. Ошибочные сценарии ==="
api POST /api/query '{"sql":"SELECT * FROM нет_такой_таблицы"}' | grep -qi 'error'
check "несуществующая таблица -> ошибка" $?
api POST /api/objects '{"table":"","geo":"","bbox":[1,2,3,4]}' | grep -qi 'error'
check "пустая таблица в запросе -> ошибка" $?

stop_server
rm -f "$BIN"

echo
echo "================================"
echo "Пройдено: $PASS, провалено: $FAIL"
[ $FAIL -eq 0 ]
