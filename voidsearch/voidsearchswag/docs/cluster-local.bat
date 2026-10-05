@echo off
REM Локальный кластер VoidSearchSwag: три полных инстанса (свой tor, база, пул).
REM Общий Bearer-токен обязателен: им же ходят пиры друг к другу.
REM Сначала один раз: setup в каждой дате (ниже закомментировано).
setlocal
set TOKEN=CHANGE_ME
set ROOT=%~dp0
set EXE=%ROOT%voidsearchswag.exe

REM Раскомментируй для первичной установки (tor+база+chromium в каждую дату):
REM set VOIDSEARCH_DATA_DIR=%ROOT%node1 & "%EXE%" setup
REM set VOIDSEARCH_DATA_DIR=%ROOT%node2 & "%EXE%" setup
REM set VOIDSEARCH_DATA_DIR=%ROOT%node3 & "%EXE%" setup

start "vss-node1" cmd /c "set VOIDSEARCH_DATA_DIR=%ROOT%node1^& set VOIDSEARCH_PEERS=http://127.0.0.1:8802,http://127.0.0.1:8803^& ""%EXE%"" run --http 127.0.0.1:8801 --token %TOKEN%"
start "vss-node2" cmd /c "set VOIDSEARCH_DATA_DIR=%ROOT%node2^& set VOIDSEARCH_PEERS=http://127.0.0.1:8801,http://127.0.0.1:8803^& ""%EXE%"" run --http 127.0.0.1:8802 --token %TOKEN%"
start "vss-node3" cmd /c "set VOIDSEARCH_DATA_DIR=%ROOT%node3^& set VOIDSEARCH_PEERS=http://127.0.0.1:8801,http://127.0.0.1:8802^& ""%EXE%"" run --http 127.0.0.1:8803 --token %TOKEN%"

echo Ноды: :8801 :8802 :8803. Проверка: peer_list на любой.
