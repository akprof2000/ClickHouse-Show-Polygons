#!/bin/bash
# Полная сборка: фронтенд (esbuild) + exe (go) с версией из файла VERSION
set -e
cd "$(dirname "$0")"
VER=$(tr -d ' \r\n' < VERSION)
echo "Версия: $VER"
(cd web && npx esbuild app.js --bundle --minify --format=iife --outfile=dist/bundle.js --loader:.ts=ts && cp index.html dist/)
go build -ldflags "-s -w -X main.version=$VER" -o chviewer.exe .
echo "Готово: chviewer.exe v$VER"
