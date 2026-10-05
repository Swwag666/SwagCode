# Windows-служба для VoidSearchSwag

Два варианта: штатный `sc.exe` (простой, без автоперезапуска при падении)
или NSSM (перезапуск, логи, зависимости). В обоих сначала прогони
`voidsearchswag setup` от своего пользователя: tor, база и chromium
должны уже лежать на диске.

## Токен: файлом, не в командной строке

Вариант с `--token` в argv оставлен только для локальных проб:
`tasklist /v` и WMI показывают командную строку любому локальному
пользователю, секрет уходит вместе с ней. Для службы токен живёт в файле,
который читает только владелец. Сгенерируй токен PowerShell и запиши в файл
(перенос строки обрезается):

```bat
powershell -NoProfile -Command "[Convert]::ToBase64String((1..32|%%{'{0:x2}' -f (Get-Random -Max 256)}))" > C:\voidsearchswag\secrets\http-token.txt
powershell -NoProfile -Command "(Get-Content C:\voidsearchswag\secrets\http-token.txt -Raw).Trim() | Set-Content C:\voidsearchswag\secrets\http-token.txt -NoNewline"
icacls C:\voidsearchswag\secrets\http-token.txt /inheritance:r /grant:r "%USERNAME%:F"
```

Дальше оба варианта ниже передают `--token-file` вместо `--token`.

## Вариант A: sc.exe (встроено в Windows)

Консоль администратора:

```bat
sc create voidsearchswag binPath= "\"C:\voidsearchswag\voidsearchswag.exe\" run --http 127.0.0.1:8800 --token-file C:\voidsearchswag\secrets\http-token.txt" start= auto
sc description voidsearchswag "Self-hosted MCP search engine"
```

Переменные окружения служба берёт системные, поэтому задай их заранее:

```bat
setx VOIDSEARCH_DATA_DIR "C:\voidsearchswag\data" /M
setx VOIDSEARCH_HUNT_BG "true" /M
setx VOIDSEARCH_DISCOVER_BG "true" /M
```

Старт/стоп:

```bat
sc start voidsearchswag
sc stop voidsearchswag
```

Минусы: нет перезапуска при падении, логи только в Event Log.
Tor-демон - дочерний процесс бинаря и умирает вместе со службой.

Служба работает от LocalSystem. Понизить до выделенной учётки можно так
(доступ к каталогам данных и секрету - только ей):

```bat
net user vsssvc /add
icacls C:\voidsearchswag\data /grant vsssvc:(OI)(CI)F
icacls C:\voidsearchswag\secrets\http-token.txt /grant vsssvc:R
sc config voidsearchswag obj= ".\vsssvc" password= "пароль-учётки"
```

## Вариант B: NSSM (рекомендуется для прод)

```bat
nssm install voidsearchswag "C:\voidsearchswag\voidsearchswag.exe" "run --http 127.0.0.1:8800 --token-file C:\voidsearchswag\secrets\http-token.txt"
nssm set voidsearchswag AppDirectory C:\voidsearchswag
nssm set voidsearchswag AppEnvironmentExtra VOIDSEARCH_DATA_DIR=C:\voidsearchswag\data VOIDSEARCH_HUNT_BG=true VOIDSEARCH_DISCOVER_BG=true
nssm set voidsearchswag Start SERVICE_AUTO_START
nssm set voidsearchswag ObjectName .\vsssvc пароль-учётки
nssm start voidsearchswag
```

NSSM сам перезапускает упавший процесс и ведёт ротацию логов
(`nssm set voidsearchswag AppStdout C:\voidsearchswag\logs\service.log`).

## Проверка

Токен из файла (без переноса строки в конце):

```bat
set /p TOK=<C:\voidsearchswag\secrets\http-token.txt
curl -H "Authorization: Bearer %TOK%" http://127.0.0.1:8800/health
```

Ответ `ok` - сервер поднят, агент может подключаться.
Проверь, что секрет не светится в командной строке процесса:

```bat
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='voidsearchswag.exe'\" | Select-Object -ExpandProperty CommandLine"
```

Фоновые циклы видны в логе службы: `фон: охота каждые 10m`.
