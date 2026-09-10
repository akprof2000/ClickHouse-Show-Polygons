#!/bin/bash
# Полная сборка: фронтенд (esbuild) + exe (go) с версией из файла VERSION
set -e
cd "$(dirname "$0")"
VER=$(tr -d ' \r\n' < VERSION)
echo "Версия: $VER"
# сборка фронтенда идёт через npm run build: там же собирается отдельный
# файл воркера maplibre (см. web/package.json) — без него слои не рисуются
(cd web && npm run build && cp index.html dist/)
go build -ldflags "-s -w -X main.version=$VER" -o chviewer.exe .
echo "Готово: chviewer.exe v$VER"
