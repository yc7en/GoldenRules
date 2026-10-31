@echo off
setlocal
rem Management platform platformd. --cors-origin is mandatory: without it the
rem browser cannot read responses and the UI shows both status dots red with
rem NETWORK_UNREACHABLE.
rem
rem ASCII-only on purpose: this file's stdout is redirected into the service
rem console's log file, which is read as UTF-8.
cd /d "%~dp0.."
set "ROOT=%CD%"
if not defined ENGINE_ADDR set "ENGINE_ADDR=127.0.0.1:18080"
if not defined PLATFORM_ADDR set "PLATFORM_ADDR=127.0.0.1:18081"
if not defined RELOAD_TOKEN set "RELOAD_TOKEN=devtoken"
if not defined LOG_KEY set "LOG_KEY=00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
if not defined DATA_DIR set "DATA_DIR=%ROOT%\.dev\data"

set "PLATFORM=%ROOT%\.dev\bin\platformd.exe"
if not exist "%PLATFORM%" (
    echo [platform] missing %PLATFORM%
    echo [platform] run scripts\build.bat first
    if /i not "%~1"=="--no-pause" pause
    exit /b 1
)
if not exist "%DATA_DIR%" mkdir "%DATA_DIR%"

echo [platform] %PLATFORM_ADDR% data=%DATA_DIR% engine=http://%ENGINE_ADDR%
"%PLATFORM%" ^
    --addr "%PLATFORM_ADDR%" ^
    --data "%DATA_DIR%" ^
    --seed "%ROOT%\backend\fixtures" ^
    --engine-url "http://%ENGINE_ADDR%" ^
    --engine-token "%RELOAD_TOKEN%" ^
    --log-key "%LOG_KEY%" ^
    --cors-origin "*"
exit /b %ERRORLEVEL%
