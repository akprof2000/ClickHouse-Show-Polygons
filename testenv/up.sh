#!/bin/bash
# Поднимает тестовый ClickHouse (контейнер ch-test) для test.sh: native 9000,
# native+TLS 9440 с самоподписанным сертификатом, пользователь default /
# test123, тестовые таблицы из setup.sql. Используется и локально, и в
# GitHub Actions — стенд в обоих случаях один и тот же.
set -euo pipefail
cd "$(dirname "$0")"
IMAGE="${CH_IMAGE:-clickhouse/clickhouse-server:latest}"

# сертификат с subjectAltName: без него режим tls: ca не проверить —
# современный Go не смотрит на CN
if [ ! -f server.crt ] || [ ! -f server.key ]; then
  openssl req -x509 -newkey rsa:2048 -keyout server.key -out server.crt \
    -days 365 -nodes -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
  echo "сертификат создан"
fi
# ClickHouse внутри контейнера читает ключ от своего пользователя
chmod 644 server.key server.crt

# git-bash на Windows: docker нужен windows-вид пути
DIR="$(pwd)"
command -v cygpath >/dev/null 2>&1 && DIR="$(cygpath -m "$DIR")"

docker rm -f ch-test >/dev/null 2>&1 || true
MSYS_NO_PATHCONV=1 docker run -d --name ch-test \
  -p 8123:8123 -p 8443:8443 -p 9000:9000 -p 9440:9440 \
  -e CLICKHOUSE_PASSWORD=test123 \
  -v "$DIR/ssl.xml:/etc/clickhouse-server/config.d/ssl.xml:ro" \
  -v "$DIR/server.crt:/etc/clickhouse-server/certs/server.crt:ro" \
  -v "$DIR/server.key:/etc/clickhouse-server/certs/server.key:ro" \
  "$IMAGE" >/dev/null

echo -n "жду ClickHouse"
for _ in $(seq 1 60); do
  if curl -s --max-time 2 http://localhost:8123/ping 2>/dev/null | grep -q Ok; then
    echo " — готов"
    break
  fi
  echo -n "."; sleep 2
done
curl -s --max-time 2 http://localhost:8123/ping | grep -q Ok || { echo; echo "ClickHouse не поднялся"; docker logs --tail 40 ch-test; exit 1; }

docker exec -i ch-test clickhouse-client --password test123 --multiquery < setup.sql
echo "строк в emr_ch.tbl_polygons_bmt: $(docker exec ch-test clickhouse-client --password test123 -q 'SELECT count() FROM emr_ch.tbl_polygons_bmt')"
