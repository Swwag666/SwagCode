#!/bin/sh
# Локальный кластер VoidSearchSwag: три полных инстанса (свой tor, база, пул).
# Общий Bearer-токен обязателен: им же ходят пиры друг к другу.
TOKEN="CHANGE_ME"
ROOT="$(cd "$(dirname "$0")" && pwd)"
EXE="$ROOT/voidsearchswag"

# Первичная установка (один раз): tor+база+chromium в каждую дату.
# VOIDSEARCH_DATA_DIR="$ROOT/node1" "$EXE" setup
# VOIDSEARCH_DATA_DIR="$ROOT/node2" "$EXE" setup
# VOIDSEARCH_DATA_DIR="$ROOT/node3" "$EXE" setup

mkdir -p "$ROOT/node1" "$ROOT/node2" "$ROOT/node3"

VOIDSEARCH_DATA_DIR="$ROOT/node1" \
VOIDSEARCH_PEERS="http://127.0.0.1:8802,http://127.0.0.1:8803" \
  "$EXE" run --http 127.0.0.1:8801 --token "$TOKEN" &
VOIDSEARCH_DATA_DIR="$ROOT/node2" \
VOIDSEARCH_PEERS="http://127.0.0.1:8801,http://127.0.0.1:8803" \
  "$EXE" run --http 127.0.0.1:8802 --token "$TOKEN" &
VOIDSEARCH_DATA_DIR="$ROOT/node3" \
VOIDSEARCH_PEERS="http://127.0.0.1:8801,http://127.0.0.1:8802" \
  "$EXE" run --http 127.0.0.1:8803 --token "$TOKEN" &

echo "Ноды: :8801 :8802 :8803. Проверка: peer_list на любой."
wait
