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
rem Which rule package to load, in order of precedence:
rem   1. PKG, when set explicitly (set PKG=D:\somewhere\rule-package.json)
rem   2. .dev\pkg\rule-package.json, when the platform has exported one
rem   3. the fixture package (4 rule sets, what the engine's own tests use)
rem Step 2 exists so that restarting the dev stack reproduces the package that
rem was just verified in the editor: trial runs the *loaded* package, so a
rem fixture-only engine answers VERSION_RULESET_NOT_FOUND for every rule set
rem that only exists in the platform. That error reads like a platform bug but
rem is only a stale package. Delete .dev\pkg to fall back to fixtures.
if not defined PKG if exist "%ROOT%\.dev\pkg\rule-package.json" (
    set "PKG=%ROOT%\.dev\pkg\rule-package.json"
    set "PKG_ORIGIN=local export under .dev\pkg"
)
if not defined PKG (
    set "PKG=%ROOT%\backend\fixtures\rule-package.json"
    set "PKG_ORIGIN=fixtures"
)

set "ENGINE=%ROOT%\backend\target\release\gr-server.exe"
if not exist "%ENGINE%" (
    echo [engine] missing %ENGINE%
    echo [engine] run scripts\build.bat first
    if /i not "%~1"=="--no-pause" pause
    exit /b 1
)

echo [engine] %ENGINE_ADDR% package=%PKG%
echo [engine] package source: %PKG_ORIGIN%
"%ENGINE%" ^
    --addr "%ENGINE_ADDR%" ^
    --facts "%ROOT%\backend\fixtures\fact-catalog.json" ^
    --package "%PKG%" ^
    --providers "%ROOT%\backend\fixtures\providers.json" ^
    --reload-token "%RELOAD_TOKEN%"
exit /b %ERRORLEVEL%
