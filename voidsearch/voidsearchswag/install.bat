@echo off
setlocal enabledelayedexpansion
cd /d "%~dp0"

echo === VoidSearchSwag one-click install ===
echo.

rem ============================================================
rem Mode 1: release branch - a ready binary sits next to this file.
rem Mode 2: sources - the binary is built locally via go build.
rem Both: setup (downloads Tor), token file, generated start.bat.
rem Token never goes into argv (stage 156 rule): start.bat uses
rem VOIDSEARCH_HTTP_TOKEN_FILE. SHA256 of a release binary is
rem verified against sums.txt before it is trusted.
rem ============================================================

set "BIN=voidsearchswag.exe"

if exist "voidsearchswag-windows-amd64.exe" (
    if not exist "%BIN%" (
        echo [1/4] found a release build, verifying its checksum...
        for /f "tokens=1" %%H in ('certutil -hashfile "voidsearchswag-windows-amd64.exe" SHA256 ^| findstr /r /i "^[0-9a-f][0-9a-f]*$"') do set "ACT=%%H"
        if exist "sums.txt" (
            if not "!ACT!"=="" (
                findstr /i /c:"!ACT!" "sums.txt" >nul
                if errorlevel 1 (
                    echo ERROR: checksum mismatch - the file is corrupt, download again.
                    exit /b 1
                )
                echo       checksum ok, the binary is healthy.
            )
        )
        copy /y "voidsearchswag-windows-amd64.exe" "%BIN%" >nul
        set "MODE=release"
    ) else (
        echo [1/4] exe is already here.
        set "MODE=local"
    )
) else (
    if exist "%BIN%" (
        echo [1/4] exe is already here.
        set "MODE=local"
    ) else (
        echo [1/4] no prebuilt binary, building from sources...
        where go >nul 2>&1
        if errorlevel 1 (
            echo       Go is not found. Trying winget...
            where winget >nul 2>&1
            if errorlevel 1 (
                echo ERROR: neither Go nor winget is available.
                echo       Install Go from https://go.dev/dl/
                echo       and run install.bat again.
                exit /b 1
            )
            winget install --id GoLang.Go --accept-source-agreements --accept-package-agreements
            if errorlevel 1 (
                echo ERROR: winget failed to install Go. Install it manually: https://go.dev/dl/
                exit /b 1
            )
            echo       Go installed. Close this window, open a new one and run install.bat again.
            exit /b 0
        )
        go build -trimpath -o "%BIN%" .\cmd\voidsearchswag
        if errorlevel 1 (
            echo ERROR: build failed.
            exit /b 1
        )
        echo       built.
        set "MODE=source"
    )
)

echo.
echo [2/4] build version:
"%BIN%" version
echo.

echo [3/4] setup: downloading Tor Expert Bundle (may take a couple of minutes)...
"%BIN%" setup
if errorlevel 1 (
    echo.
    echo ERROR: setup could not download Tor. The official dist.torproject.org
    echo        regularly answers 503. Point VOIDSEARCH_TOR_DIST to a mirror and retry:
    echo            set VOIDSEARCH_TOR_DIST=https://your-mirror/torbrowser/
    echo            install.bat
    echo        Or place a tor binary yourself: docs\env.md, VOIDSEARCH_TOR_BINARY.
    exit /b 1
)
echo       Tor is in place.

echo.
echo [4/4] token and start.bat...
if not exist "token.txt" (
    for /f %%T in ('powershell -NoProfile -Command "[guid]::NewGuid().ToString('N')"') do set "TOKEN=%%T"
    echo !TOKEN!> token.txt
    echo       token written to token.txt
) else (
    echo       token.txt already exists, keeping it.
)

(
    echo @echo off
    echo cd /d "%%~dp0"
    echo if not defined VOIDSEARCH_HTTP_ADDR set "VOIDSEARCH_HTTP_ADDR=127.0.0.1:3333"
    echo if not defined VOIDSEARCH_HTTP_TOKEN_FILE set "VOIDSEARCH_HTTP_TOKEN_FILE=%%~dp0token.txt"
    echo "%%~dp0voidsearchswag.exe" run
    echo pause
) > start.bat

echo       start.bat is ready.
echo.
echo === Done. Run start.bat - the server comes up on ===
echo === 127.0.0.1:3333 with the token from token.txt. ===
echo === Read token.txt and hand the token to your MCP client. ===
echo.
endlocal
