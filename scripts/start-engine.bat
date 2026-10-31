@echo off
setlocal
rem Decision service gr-server. All four path args matter: without --providers
rem /v1/assemble returns 503, and without --reload-token the platform publish
rem gate fails closed and blocks every publish. Both look like bugs, not config.
rem
rem This file is deliberately ASCII-only: its stdout goes into the service
rem console's log file, which is read as UTF-8. A GBK line here would be the
rem one garbled line in an otherwise readable log. Keep Chinese out of it.
cd /d "%~dp0.."
set "ROOT=%CD%"
if not defined ENGINE_ADDR set "ENGINE_ADDR=127.0.0.1:18080"
if not defined RELOAD_TOKEN set "RELOAD_TOKEN=devtoken"

set "ENGINE=%ROOT%\backend\target\release\gr-server.exe"
if not exist "%ENGINE%" (
    echo [engine] missing %ENGINE%
    echo [engine] run scripts\build.bat first
    if /i not "%~1"=="--no-pause" pause
    exit /b 1
)

echo [engine] %ENGINE_ADDR%
"%ENGINE%" ^
    --addr "%ENGINE_ADDR%" ^
    --facts "%ROOT%\backend\fixtures\fact-catalog.json" ^
    --package "%ROOT%\backend\fixtures\rule-package.json" ^
    --providers "%ROOT%\backend\fixtures\providers.json" ^
    --reload-token "%RELOAD_TOKEN%"
exit /b %ERRORLEVEL%
