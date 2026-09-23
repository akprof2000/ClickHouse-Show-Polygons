#!/bin/sh
# chviewer — веб-сервер: полигоны из ClickHouse на карте OpenStreetMap.
#
# Отредактируйте значения ниже (или переопределите переменными окружения)
# и запустите: ./serve.sh
# При первом запуске рядом создаётся chviewer.yaml из этих значений; дальше
# правьте сам chviewer.yaml (полный пример с комментариями: chviewer -init).
#
# Пароль ClickHouse НЕ хранится в файлах — задайте переменную окружения:
#   CH_PASSWORD=secret ./serve.sh
# Либо логин и пароль берутся из PAM (тогда CH_USER/CH_PASSWORD не нужны):
#   PAM_SERVER=https://pam.example.com PAM_TOKEN=... \
#   PAM_SECRET=/Инфраструктура/ClickHouse/viewer ./serve.sh
#
# Токен доступа к странице задаётся так же, переменной окружения:
#   CHVIEWER_TOKEN=... ./serve.sh
set -eu
DIR="$(cd "$(dirname "$0")" && pwd)"

# --- сервер -----------------------------------------------------------------
LISTEN="${LISTEN:-:8081}"                     # адрес сервера; 127.0.0.1:8081 — только локально
DATA_DIR="${DATA_DIR:-$DIR/data}"             # здесь хранятся общие шаблоны слоёв
TITLE="${TITLE:-ClickHouse Show Polygons}"
AUTH_TOKEN="${AUTH_TOKEN:-}"                  # токен прямо в конфиге (лучше — CHVIEWER_TOKEN)
AUTH_TOKEN_ENV="${AUTH_TOKEN_ENV:-CHVIEWER_TOKEN}"
# HTTPS самого сервера (браузер → chviewer); пусто = обычный HTTP
HTTPS_CERT="${HTTPS_CERT:-}"                  # /etc/pki/tls/certs/chviewer.pem
HTTPS_KEY="${HTTPS_KEY:-}"                    # /etc/pki/tls/private/chviewer.key

# --- ClickHouse -------------------------------------------------------------
CH_URL="${CH_URL:-http://localhost:8123/}"    # http(s)-адрес ClickHouse
CH_INSECURE="${CH_INSECURE:-false}"           # true = не проверять сертификат ClickHouse
CH_TIMEOUT="${CH_TIMEOUT:-120s}"
CH_USER="${CH_USER:-default}"
CH_PASSWORD_ENV="${CH_PASSWORD_ENV:-CH_PASSWORD}"  # имя переменной окружения с паролем
# вход через PAM вместо пароля: если PAM_SECRET не пуст, логин и пароль
# берутся из записи PAM, а CH_USER/CH_PASSWORD_ENV не используются
PAM_SECRET="${PAM_SECRET:-}"                  # /Группа/Подгруппа/запись
PAM_SERVER="${PAM_SERVER:-}"                  # https://pam.example.com
PAM_TOKEN_ENV="${PAM_TOKEN_ENV:-PAM_TOKEN}"   # имя переменной окружения с AAPM-токеном
PAM_COMMENT="${PAM_COMMENT:-chviewer}"        # комментарий в журнал аудита PAM
PAM_CA="${PAM_CA:-}"                          # PEM корневого сертификата PAM
PAM_TLS_CERT="${PAM_TLS_CERT:-}"              # клиентский сертификат для PAM
PAM_TLS_KEY="${PAM_TLS_KEY:-}"
PAM_INSECURE="${PAM_INSECURE:-false}"
PAM_TIMEOUT="${PAM_TIMEOUT:-10s}"
PAM_TTL="${PAM_TTL:-10m}"

# ---------------------------------------------------------------------------
CONFIG="${CONFIG:-$DIR/chviewer.yaml}"
BIN="$DIR/chviewer"
[ -x "$BIN" ] || BIN="$BIN.exe"      # git-bash / MSYS на Windows
[ -x "$BIN" ] || BIN="chviewer"      # иначе ищем в PATH

# git-bash / MSYS на Windows: /c/... -> C:/... для Go-программы
if command -v cygpath >/dev/null 2>&1; then
  DATA_DIR="$(cygpath -m "$DATA_DIR")"
  [ -n "$HTTPS_CERT" ] && HTTPS_CERT="$(cygpath -m "$HTTPS_CERT")"
  [ -n "$HTTPS_KEY" ]  && HTTPS_KEY="$(cygpath -m "$HTTPS_KEY")"
  [ -n "$PAM_CA" ]     && PAM_CA="$(cygpath -m "$PAM_CA")"
  [ -n "$PAM_TLS_CERT" ] && PAM_TLS_CERT="$(cygpath -m "$PAM_TLS_CERT")"
  [ -n "$PAM_TLS_KEY" ]  && PAM_TLS_KEY="$(cygpath -m "$PAM_TLS_KEY")"
fi

if [ ! -f "$CONFIG" ]; then
  # при входе через PAM логин и пароль приходят из записи, поэтому
  # user/password_env в конфиге остаются пустыми
  ch_user="$CH_USER"; ch_password_env="$CH_PASSWORD_ENV"
  if [ -n "$PAM_SECRET" ]; then ch_user=""; ch_password_env=""; fi
  ca_list=""; [ -n "$PAM_CA" ] && ca_list="\"$PAM_CA\""
  cat > "$CONFIG" <<YAML
# Создано serve.sh из значений по умолчанию. Полный пример: chviewer -init
listen: "$LISTEN"
title: "$TITLE"
data_dir: "$DATA_DIR"
auth:
  token: "$AUTH_TOKEN"
  token_env: "$AUTH_TOKEN_ENV"
clickhouse:
  url: "$CH_URL"
  insecure: $CH_INSECURE
  timeout: $CH_TIMEOUT
  user: "$ch_user"
  password_env: "$ch_password_env"
  pam:
    secret: "$PAM_SECRET"
    server: "$PAM_SERVER"
    token_env: "$PAM_TOKEN_ENV"
    comment: "$PAM_COMMENT"
    ca_cert: [$ca_list]
    tls_cert: "$PAM_TLS_CERT"
    tls_key: "$PAM_TLS_KEY"
    insecure: $PAM_INSECURE
    timeout: $PAM_TIMEOUT
    ttl: $PAM_TTL
tls:
  cert_file: "$HTTPS_CERT"
  key_file: "$HTTPS_KEY"
YAML
  echo "создан $CONFIG"
fi
exec "$BIN" -config "$CONFIG" "$@"
